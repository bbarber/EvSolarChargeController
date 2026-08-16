package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	zmq "github.com/pebbe/zmq4"
)

// Subscriber consumes vehicle records from fleet-telemetry's ZeroMQ PUB socket.
//
// ZeroMQ is the only one of fleet-telemetry's dispatchers that needs no broker — the alternatives
// are Kafka, Kinesis, Pub/Sub, MQTT and Redis, each of which would mean another process to run and
// watch on a 1 OCPU box. There is no HTTP dispatcher, so this is the cheapest way to get records
// out of Tesla's server.
type Subscriber struct {
	endpoint string
	topic    string
	log      *slog.Logger
}

// NewSubscriber targets the PUB socket. The endpoint is normally a loopback address: the telemetry
// server and this process share a host, and the socket should never be exposed off it.
func NewSubscriber(endpoint, topic string, log *slog.Logger) *Subscriber {
	return &Subscriber{endpoint: endpoint, topic: topic, log: log}
}

// Run subscribes and dispatches records until ctx is cancelled.
//
// Records arrive as two frames: the topic, then the protobuf payload.
func (s *Subscriber) Run(ctx context.Context, handle func(context.Context, []byte) error) error {
	socket, err := zmq.NewSocket(zmq.SUB)
	if err != nil {
		return fmt.Errorf("creating the ZMQ SUB socket: %w", err)
	}
	defer socket.Close()

	if err := socket.Connect(s.endpoint); err != nil {
		return fmt.Errorf("connecting to %s: %w", s.endpoint, err)
	}
	if err := socket.SetSubscribe(s.topic); err != nil {
		return fmt.Errorf("subscribing to %q: %w", s.topic, err)
	}

	// A receive timeout is what makes cancellation possible: without it the blocking Recv would
	// ignore ctx entirely and the process would refuse to shut down.
	if err := socket.SetRcvtimeo(time.Second); err != nil {
		return fmt.Errorf("setting the receive timeout: %w", err)
	}

	s.log.Info("subscribed to fleet-telemetry", "endpoint", s.endpoint, "topic", s.topic)

	for {
		if err := ctx.Err(); err != nil {
			return nil
		}

		frames, err := socket.RecvMessageBytes(0)
		if err != nil {
			if zmq.AsErrno(err) == zmq.Errno(zmq.ETIMEDOUT) {
				continue // Idle window; the car is asleep or has nothing to report.
			}
			if ctx.Err() != nil {
				return nil
			}
			s.log.Warn("receiving a telemetry frame", "error", err)
			continue
		}

		payload := frames[len(frames)-1]
		if len(frames) < 2 || len(payload) == 0 {
			s.log.Warn("ignoring a telemetry frame with no payload", "frames", len(frames))
			continue
		}

		if err := handle(ctx, payload); err != nil {
			s.log.Warn("handling a telemetry record", "error", err)
		}
	}
}
