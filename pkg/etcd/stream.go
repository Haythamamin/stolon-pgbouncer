package etcd

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/mvcc/mvccpb"
	kitlog "github.com/go-kit/kit/log"
)

// StreamOptions should be passed to NewStream to construct a new streaming channel
type StreamOptions struct {
	Ctx                context.Context
	Keys               []string
	PollInterval       time.Duration
	WatchRetryInterval time.Duration
	GetTimeout         time.Duration
}

// Thin interface around the etcd functions we require
type etcdGetter interface {
	Get(context.Context, string, ...clientv3.OpOption) (*clientv3.GetResponse, error)
	Watch(context.Context, string, ...clientv3.OpOption) clientv3.WatchChan
}

// NewStream accepts an etcd client with which we watch for changes to our selected keys
// and push them down the output channel. The advantages to using this interface over what
// etcd already provides is the polling interval, which ensures on boot that we receive
// the initial value, along with at polling intervals.
func NewStream(logger kitlog.Logger, client etcdGetter, opt StreamOptions) (<-chan *mvccpb.KeyValue, <-chan struct{}) {
	logger = kitlog.With(logger, "keys", strings.Join(opt.Keys, ","))
	out, done := make(chan *mvccpb.KeyValue), make(chan struct{})

	ctx, cancel := context.WithCancel(opt.Ctx)
	var wg sync.WaitGroup
	wg.Add(2)

	// Start watching etcd using the watch API, pushing each etcd change into the out stream
	go func() {
		defer cancel()
		defer wg.Done()

	Watch:
		for {
			logger.Log("event", "watch_start")
			for resp := range client.Watch(clientv3.WithRequireLeader(ctx), "/", clientv3.WithPrefix()) {
				if resp.Err() != nil {
					logger.Log("error", resp.Err(), "msg", "received error from etcd watcher")
				}

				for _, event := range resp.Events {
					if includes(opt.Keys, string(event.Kv.Key)) {
						out <- event.Kv
					}
				}
			}

			select {
			case <-ctx.Done():
				logger.Log("event", "watch_stop", "msg", "context expired, stopping stream")
				break Watch
			case <-time.After(opt.WatchRetryInterval):
				// watch again
			}
		}
	}()

	// The etcd watch API retries indefinitely, but the abstraction hides errors. By
	// manually polling for etcd changes on a regular interval we ensure we'll at least see
	// logs if the stream breaks down, as our manual get will fail.
	go func() {
		defer cancel()
		defer wg.Done()

	Poll:
		for {
			logger.Log("event", "poll_start")
			for _, key := range opt.Keys {
				getCtx, getCtxCancel := context.WithTimeout(ctx, opt.GetTimeout)
				resp, err := client.Get(getCtx, key)
				getCtxCancel()

				if err != nil {
					logger.Log("error", err, "key", key, "msg", "failed to poll etcd")
					continue
				}

				if len(resp.Kvs) == 0 {
					logger.Log("error", "poll_missing_etcd_value", "key", key,
						"msg", "key has no value (is supervise running?)")
					continue
				}

				out <- resp.Kvs[0]
			}

			select {
			case <-ctx.Done():
				logger.Log("event", "poll_stop", "msg", "context expired, stopping stream")
				break Poll
			case <-time.After(opt.PollInterval):
				// poll again
			}
		}
	}()

	// Notify the done channel once the wait group completes
	go func() {
		wg.Wait()

		close(out)
		close(done)
	}()

	return out, done
}

func includes(set []string, elem string) bool {
	for _, candidate := range set {
		if candidate == elem {
			return true
		}
	}

	return false
}
