/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package interruption

import (
	"context"
	"fmt"
	"time"

	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/metrics"

	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	sqsapi "github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/awslabs/operatorpkg/singleton"
	"go.uber.org/multierr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/karpenter/pkg/operator/injection"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"

	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/aws/karpenter-provider-aws/pkg/cache"
	interruptionevents "github.com/aws/karpenter-provider-aws/pkg/controllers/interruption/events"
	"github.com/aws/karpenter-provider-aws/pkg/controllers/interruption/messages"
	"github.com/aws/karpenter-provider-aws/pkg/providers/sqs"
)

type Action string

const (
	CordonAndDrain Action = "CordonAndDrain"
	NoAction       Action = "NoAction"
)

// Controller is an AWS interruption controller.
// It continually polls an SQS queue for events from aws.ec2 and aws.health that
// trigger node health events or node spot interruption/rebalance events.
type Controller struct {
	kubeClient                client.Client
	cloudProvider             cloudprovider.CloudProvider
	clk                       clock.Clock
	recorder                  events.Recorder
	sqsProvider               sqs.Provider
	sqsAPI                    *sqsapi.Client
	unavailableOfferingsCache *cache.UnavailableOfferings
	parser                    *EventParser
	cm                        *pretty.ChangeMonitor
}

func NewController(
	kubeClient client.Client,
	cloudProvider cloudprovider.CloudProvider,
	clk clock.Clock,
	recorder events.Recorder,
	sqsProvider sqs.Provider,
	sqsAPI *sqsapi.Client,
	unavailableOfferingsCache *cache.UnavailableOfferings,
) *Controller {
	return &Controller{
		kubeClient:                kubeClient,
		cloudProvider:             cloudProvider,
		clk:                       clk,
		recorder:                  recorder,
		sqsProvider:               sqsProvider,
		sqsAPI:                    sqsAPI,
		unavailableOfferingsCache: unavailableOfferingsCache,
		parser:                    NewEventParser(DefaultParsers...),
		cm:                        pretty.NewChangeMonitor(),
	}
}

func (c *Controller) Reconcile(ctx context.Context) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, "interruption")
	if c.sqsProvider == nil {
		prov, err := sqs.NewSQSProvider(ctx, c.sqsAPI)
		if err != nil {
			log.FromContext(ctx).Error(err, "failed to create valid sqs provider")
			return reconcile.Result{}, fmt.Errorf("creating sqs provider, %w", err)
		}
		c.sqsProvider = prov
	}
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("queue", c.sqsProvider.Name()))
	if c.cm.HasChanged(c.sqsProvider.Name(), nil) {
		log.FromContext(ctx).V(1).Info("watching interruption queue")
	}
	sqsMessages, err := c.sqsProvider.GetSQSMessages(ctx)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("getting messages from queue, %w", err)
	}
	if len(sqsMessages) == 0 {
		return reconcile.Result{RequeueAfter: singleton.RequeueImmediately}, nil
	}

	errs := make([]error, len(sqsMessages))
	workqueue.ParallelizeUntil(ctx, 10, len(sqsMessages), func(i int) {
		msg, e := c.parseMessage(sqsMessages[i])
		if e != nil {
			// If we fail to parse, then we should delete the message but still log the error
			log.FromContext(ctx).Error(e, "failed parsing interruption message")
			errs[i] = c.deleteMessage(ctx, sqsMessages[i])
			return
		}
		if e = c.handleMessage(ctx, msg); e != nil {
			errs[i] = fmt.Errorf("handling message, %w", e)
			return
		}
		errs[i] = c.deleteMessage(ctx, sqsMessages[i])
	})
	if err = multierr.Combine(errs...); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{RequeueAfter: singleton.RequeueImmediately}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("interruption").
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}

// parseMessage parses the passed SQS message into an internal Message interface
func (c *Controller) parseMessage(raw *sqstypes.Message) (messages.Message, error) {
	// No message to parse in this case
	if raw == nil || raw.Body == nil {
		return nil, fmt.Errorf("message or message body is nil")
	}
	msg, err := c.parser.Parse(*raw.Body)
	if err != nil {
		return nil, fmt.Errorf("parsing sqs message, %w", err)
	}
	return msg, nil
}

// handleMessage takes an action against every node involved in the message that is owned by a NodePool
func (c *Controller) handleMessage(ctx context.Context, msg messages.Message) (err error) {
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("messageKind", msg.Kind()))
	ReceivedMessages.Inc(map[string]string{messageTypeLabel: string(msg.Kind())})

	if msg.Kind() == messages.NoOpKind {
		return nil
	}
	for _, instanceID := range msg.EC2InstanceIDs() {
		nodeClaimList := &karpv1.NodeClaimList{}
		if e := c.kubeClient.List(ctx, nodeClaimList, client.MatchingFields{"status.instanceID": instanceID}); e != nil {
			err = multierr.Append(err, e)
			continue
		}
		if len(nodeClaimList.Items) == 0 {
			continue
		}
		for _, nodeClaim := range nodeClaimList.Items {
			nodeList := &corev1.NodeList{}
			if e := c.kubeClient.List(ctx, nodeList, client.MatchingFields{"spec.instanceID": instanceID}); e != nil {
				err = multierr.Append(err, e)
				continue
			}
			var node *corev1.Node
			if len(nodeList.Items) > 0 {
				node = &nodeList.Items[0]
			}
			if e := c.handleNodeClaim(ctx, msg, &nodeClaim, node); e != nil {
				err = multierr.Append(err, e)
			}
		}
	}
	MessageLatency.Observe(time.Since(msg.StartTime()).Seconds(), nil)
	if err != nil {
		return fmt.Errorf("acting on NodeClaims, %w", err)
	}
	return nil
}

// deleteMessage removes the passed SQS message from the queue and fires a metric for the deletion
func (c *Controller) deleteMessage(ctx context.Context, msg *sqstypes.Message) error {
	if err := c.sqsProvider.DeleteSQSMessage(ctx, msg); err != nil {
		return fmt.Errorf("deleting sqs message, %w", err)
	}
	DeletedMessages.Inc(nil)
	return nil
}

// handleNodeClaim retrieves the action for the message and then performs the appropriate action against the node
func (c *Controller) handleNodeClaim(ctx context.Context, msg messages.Message, nodeClaim *karpv1.NodeClaim, node *corev1.Node) error {
	action := actionForMessage(msg)
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("NodeClaim", klog.KObj(nodeClaim), "action", string(action)))
	if node != nil {
		ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("Node", klog.KObj(node)))
	}

	// Record metric and event for this action
	c.notifyForMessage(msg, nodeClaim, node)

	// Mark the offering as unavailable in the ICE cache since we got a spot interruption warning
	if msg.Kind() == messages.SpotInterruptionKind {
		zone := nodeClaim.Labels[corev1.LabelTopologyZone]
		instanceType := nodeClaim.Labels[corev1.LabelInstanceTypeStable]
		if zone != "" && instanceType != "" {
			c.unavailableOfferingsCache.MarkUnavailable(ctx, string(msg.Kind()), ec2types.InstanceType(instanceType), zone, karpv1.CapacityTypeSpot)
		}
	}
	if action != NoAction {
		return c.deleteNodeClaim(ctx, msg, nodeClaim, node)
	}
	return nil
}

// deleteNodeClaim removes the NodeClaim from the api-server
func (c *Controller) deleteNodeClaim(ctx context.Context, msg messages.Message, nodeClaim *karpv1.NodeClaim, node *corev1.Node) error {
	if !nodeClaim.DeletionTimestamp.IsZero() {
		return nil
	}
	if err := c.kubeClient.Delete(ctx, nodeClaim); err != nil {
		return client.IgnoreNotFound(fmt.Errorf("deleting the node on interruption message, %w", err))
	}
	log.FromContext(ctx).Info("initiating delete from interruption message")
	c.recorder.Publish(interruptionevents.TerminatingOnInterruption(node, nodeClaim)...)
	metrics.NodeClaimsDisruptedTotal.Inc(map[string]string{
		metrics.ReasonLabel:       string(msg.Kind()),
		metrics.NodePoolLabel:     nodeClaim.Labels[karpv1.NodePoolLabelKey],
		metrics.CapacityTypeLabel: nodeClaim.Labels[karpv1.CapacityTypeLabelKey],
	})
	return nil
}

// notifyForMessage publishes the relevant alert based on the message kind
func (c *Controller) notifyForMessage(msg messages.Message, nodeClaim *karpv1.NodeClaim, n *corev1.Node) {
	switch msg.Kind() {
	case messages.RebalanceRecommendationKind:
		c.recorder.Publish(interruptionevents.RebalanceRecommendation(n, nodeClaim)...)

	case messages.ScheduledChangeKind:
		c.recorder.Publish(interruptionevents.Unhealthy(n, nodeClaim)...)

	case messages.SpotInterruptionKind:
		c.recorder.Publish(interruptionevents.SpotInterrupted(n, nodeClaim)...)

	case messages.InstanceStoppedKind:
		c.recorder.Publish(interruptionevents.Stopping(n, nodeClaim)...)

	case messages.InstanceTerminatedKind:
		c.recorder.Publish(interruptionevents.Terminating(n, nodeClaim)...)

	default:
	}
}

func actionForMessage(msg messages.Message) Action {
	switch msg.Kind() {
	case messages.ScheduledChangeKind, messages.SpotInterruptionKind, messages.InstanceStoppedKind, messages.InstanceTerminatedKind:
		return CordonAndDrain
	default:
		return NoAction
	}
}
