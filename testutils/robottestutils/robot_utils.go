// Package robottestutils provides helper functions in testing
package robottestutils

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.viam.com/test"
	"go.viam.com/utils"
	"go.viam.com/utils/testutils"

	"go.viam.com/rdk/robot/client"
	"go.viam.com/rdk/robot/web"
	weboptions "go.viam.com/rdk/robot/web/options"
)

// CreateBaseOptionsAndListener creates a new web options with random port as listener.
func CreateBaseOptionsAndListener(tb testing.TB) (weboptions.Options, net.Listener, string) {
	tb.Helper()
	var listener net.Listener = testutils.ReserveRandomListener(tb)
	options := weboptions.New()
	options.Network.BindAddress = ""
	options.Network.Listener = listener
	addr := listener.Addr().String()
	return options, listener, addr
}

// NewRobotClient creates a new robot client with a certain address.
func NewRobotClient(tb testing.TB, logger *zap.SugaredLogger, addr string, dur time.Duration) *client.RobotClient {
	tb.Helper()
	// start robot client
	robotClient, err := client.New(
		context.Background(),
		addr,
		logger,
		client.WithRefreshEvery(dur),
		client.WithCheckConnectedEvery(5*dur),
		client.WithReconnectEvery(dur),
	)
	test.That(tb, err, test.ShouldBeNil)
	return robotClient
}

// ServeWebInBackground serves the web service in the background
func ServeWebInBackground(t *testing.T, ctx context.Context, svc web.Service, o weboptions.Options) *sync.WaitGroup {
	t.Helper()
	var activeBackgroundWorkers sync.WaitGroup
	activeBackgroundWorkers.Add(1)
	utils.PanicCapturingGo(func() {
		defer activeBackgroundWorkers.Done()
		err := svc.Serve(ctx, o)
		test.That(t, err, test.ShouldBeNil)
	})
	return &activeBackgroundWorkers
}
