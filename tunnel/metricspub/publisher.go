// Package metricspub adapts bounded tunnel aggregates to the existing Pub/Sub
// pipeline. This package is never imported by the customer-side agent.
package metricspub

import (
	"context"
	"time"

	"cloud.google.com/go/pubsub/v2"

	tunnelv1 "github.com/speakeasy-api/gram/infra/gen/gram/tunnel/v1"
	"github.com/speakeasy-api/gram/infra/pkg/gcp"
	"github.com/speakeasy-api/gram/tunnel/metrics"
)

func Publish(pub gcp.Publisher[*tunnelv1.MetricsSnapshot]) func(context.Context, metrics.Snapshot) error {
	return func(ctx context.Context, s metrics.Snapshot) error {
		message := &tunnelv1.MetricsSnapshot{SourceId: s.SourceID, ProducerId: s.ProducerID, BucketUnix: s.Bucket, Revision: s.Revision, Kind: s.Kind, ServerId: s.ServerID, Method: s.Method, ClientFamily: s.ClientFamily, Attempts: s.Attempts, Successes: s.Successes, Errors: s.Errors, Canceled: s.Canceled, Incomplete: s.Incomplete, LatencyBins: s.LatencyBins[:], Connections: s.Connections, Consumers: s.Consumers, Substreams: s.Substreams, DiagnosticsAvailable: s.DiagnosticsAvailable, TargetsUnreachable: s.TargetsUnreachable, ConnectionsOpened: s.ConnectionsOpened}
		_, err := pub.Publish(ctx, message).Get(ctx)
		return err
	}
}

// NewPublisher bounds SDK buffering as well as the collector's aggregate map.
func NewPublisher(ctx context.Context, broker gcp.PublisherBroker) (gcp.Publisher[*tunnelv1.MetricsSnapshot], error) {
	settings := pubsub.DefaultPublishSettings
	settings.DelayThreshold = 100 * time.Millisecond
	settings.CountThreshold = 100
	settings.ByteThreshold = 1 << 20
	settings.FlowControlSettings = pubsub.FlowControlSettings{MaxOutstandingMessages: 1000, MaxOutstandingBytes: 16 << 20, LimitExceededBehavior: pubsub.FlowControlSignalError}
	return gcp.PubSubPublisherForMessage(ctx, broker, &tunnelv1.MetricsSnapshot{}, gcp.WithPubSubPublishSettings(&settings))
}
