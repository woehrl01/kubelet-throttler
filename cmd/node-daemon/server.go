package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"woehrl01/pod-pacemaker/pkg/podaccessor"
	"woehrl01/pod-pacemaker/pkg/throttler"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	pb "woehrl01/pod-pacemaker/proto"

	"google.golang.org/grpc"
)

var (
	waitTimeHistogram = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "pod_pacemaker_wait_duration_seconds",
		Help:    "Duration of wait requests",
		Buckets: prometheus.ExponentialBucketsRange(0.1, 60, 5),
	})
	podNotFoundCounter = promauto.NewCounter(prometheus.CounterOpts{
		Name: "pod_pacemaker_pod_not_found",
		Help: "Pod not found",
	})
	waitFailedCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pod_pacemaker_wait_failed",
		Help: "Wait failed",
	}, []string{"reason"})
)

type podLimitService struct {
	pb.UnimplementedPodLimiterServer
	throttler   throttler.Throttler
	podAccessor podaccessor.PodAccessor
	options     Options
	inflight    *NamedLocks
}

type Options struct {
	Socket                string
	SkipDaemonSets        bool
	TrackInflightRequests bool
}

var _ pb.PodLimiterServer = &podLimitService{}

func NewPodLimitersServer(throttler throttler.Throttler, podAccessor podaccessor.PodAccessor, o Options) *podLimitService {
	return &podLimitService{
		throttler:   throttler,
		podAccessor: podAccessor,
		options:     o,
		inflight:    NewNamedLocks(),
	}
}

func (s *podLimitService) Wait(ctx context.Context, in *pb.WaitRequest) (*pb.WaitResponse, error) {
	log.Debugf("Received: %v", in.GetSlotName())

	slotId := in.GetSlotName()

	if s.options.TrackInflightRequests {
		if acquired := s.inflight.TryAcquire(slotId); !acquired {
			return nil, fmt.Errorf("slot %s already awaited for", slotId)
		}
	}
	defer s.inflight.Release(slotId)

	startTime := time.Now()

	var pod *corev1.Pod
	wait.PollUntilContextCancel(ctx, 500*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		p, err := s.podAccessor.GetPodByKey(slotId)
		if err != nil {
			podNotFoundCounter.Inc()
			return false, nil
		}
		if p == nil {
			podNotFoundCounter.Inc()
			return false, nil
		}
		pod = p
		return true, nil
	})
	if pod == nil {
		log.Warnf("Failed to get pod: %v", slotId)
		waitFailedCounter.WithLabelValues("pod_not_found").Inc()
		return &pb.WaitResponse{Success: false, Message: "Failed to get pod"}, nil
	}

	data := throttler.Data{
		Pod: pod,
	}

	if s.options.SkipDaemonSets && pod.ObjectMeta.OwnerReferences != nil && len(pod.ObjectMeta.OwnerReferences) > 0 && pod.ObjectMeta.OwnerReferences[0].Kind == "DaemonSet" {
		log.Debugf("Skipping daemonset: %v", pod.ObjectMeta.Name)
		return &pb.WaitResponse{Success: true, Message: "Skipped daemonset"}, nil
	}

	if err := s.throttler.AquireSlot(ctx, slotId, data); err != nil {
		log.Debugf("Failed to acquire lock: %v", err)
		waitFailedCounter.WithLabelValues("failed_to_acquire_lock").Inc()
		return &pb.WaitResponse{Success: false, Message: "Failed to acquire lock in time"}, nil
	}

	if ctx.Err() != nil {
		log.Debugf("Context cancelled")
		waitFailedCounter.WithLabelValues("context_cancelled").Inc()
		return &pb.WaitResponse{Success: false, Message: "Context cancelled"}, nil
	}

	duration := time.Since(startTime)
	log.WithFields(log.Fields{
		"duration": duration,
		"slot":     slotId,
	}).Debug("Acquired slot")

	waitTimeHistogram.Observe(duration.Seconds())

	return &pb.WaitResponse{Success: true, Message: "Waited successfully"}, nil
}

func startGrpcServer(throttler throttler.Throttler, o Options, podAccessor podaccessor.PodAccessor, stopper <-chan struct{}) {
	_ = syscall.Unlink(o.Socket) // clean up old socket and ignore errors

	lis, err := net.Listen("unix", o.Socket)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	// Unix sockets must be unlink()ed before being reused again.
	// Handle common process-killing signals so we can gracefully shut down:
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, os.Kill, syscall.SIGTERM)
	go func(c chan os.Signal) {
		// Wait for a SIGINT or SIGKILL:
		sig := <-c
		log.Printf("Caught signal %s: shutting down.", sig)
		// Stop listening (and unlink the socket if unix type):
		lis.Close()
		// And we're done:
		os.Exit(0)
	}(sigc)

	s := grpc.NewServer()

	go func() {
		<-stopper
		s.GracefulStop()
	}()

	service := NewPodLimitersServer(throttler, podAccessor, o)

	pb.RegisterPodLimiterServer(s, service)

	log.Printf("server listening at %v", lis.Addr())
	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}

type NamedLocks struct {
	locks map[string]bool
	mux   *sync.Mutex
}

func NewNamedLocks() *NamedLocks {
	return &NamedLocks{
		locks: make(map[string]bool),
		mux:   &sync.Mutex{},
	}
}

func (vl *NamedLocks) TryAcquire(name string) bool {
	vl.mux.Lock()
	defer vl.mux.Unlock()
	if _, exists := vl.locks[name]; exists {
		return false
	}
	vl.locks[name] = true
	return true
}

func (vl *NamedLocks) Release(name string) {
	vl.mux.Lock()
	defer vl.mux.Unlock()
	delete(vl.locks, name)
}
