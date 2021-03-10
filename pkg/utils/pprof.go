// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package utils

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"

	"github.com/pingcap/errors"

	"github.com/pingcap/br/pkg/lightning/common"

	// #nosec
	// register HTTP handler for /debug/pprof
	_ "net/http/pprof"

	"github.com/pingcap/failpoint"
	"github.com/pingcap/log"
	"go.uber.org/zap"
)

var (
	startedPProf = ""
	mu           sync.Mutex
)

func listen(statusAddr string) (net.Listener, error) {
	mu.Lock()
	defer mu.Unlock()
	if startedPProf != "" {
		log.Warn("Try to start pprof when it has been started, nothing will happen", zap.String("address", startedPProf))
		return nil, nil
	}
	failpoint.Inject("determined-pprof-port", func(v failpoint.Value) {
		port := v.(int)
		statusAddr = fmt.Sprintf(":%d", port)
		log.Info("injecting failpoint, pprof will start at determined port", zap.Int("port", port))
	})
	listener, err := net.Listen("tcp", statusAddr)
	if err != nil {
		log.Warn("failed to start pprof", zap.String("addr", statusAddr), zap.Error(err))
		return nil, errors.Trace(err)
	}
	startedPProf = listener.Addr().String()
	log.Info("bound pprof to addr", zap.String("addr", startedPProf))
	_, _ = fmt.Fprintf(os.Stderr, "bound pprof to addr %s\n", startedPProf)
	return listener, nil
}

// StartPProfListener forks a new goroutine listening on specified port and provide pprof info.
func StartPProfListener(statusAddr string, wrapper *common.TLS) error {
	listener, err := listen(statusAddr)
	if err != nil {
		return err
	}

	if listener != nil {
		go func() {
			if e := http.Serve(wrapper.WrapListener(listener), nil); e != nil {
				log.Warn("failed to serve pprof", zap.String("addr", startedPProf), zap.Error(e))
				mu.Lock()
				startedPProf = ""
				mu.Unlock()
				return
			}
		}()
	}
	return nil
}
