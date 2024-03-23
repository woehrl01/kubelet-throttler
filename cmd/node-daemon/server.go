package main

import (
	"context"
	"fmt"
	"net"
	"time"

	"woehrl01/pod-pacemaker/pkg/throttler"

	log "github.com/sirupsen/logrus"

	pb "woehrl01/pod-pacemaker/proto"

	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"

	"google.golang.org/grpc"
)

type podLimitService struct {
	pb.UnimplementedPodLimiterServer
	throttler throttler.Throttler
}

var _ pb.PodLimiterServer = &podLimitService{}

func NewPodLimitersServer(throttler throttler.Throttler) *podLimitService {
	return &podLimitService{
		throttler: throttler,
	}
}

func (s *podLimitService) Wait(ctx context.Context, in *pb.WaitRequest) (*pb.WaitResponse, error) {
	log.Debugf("Received: %v", in.GetSlotName())
	startTime := time.Now()
	if err := s.throttler.AquireSlot(ctx, in.GetSlotName(), throttler.Data{}); err != nil {
		log.Infof("Failed to acquire lock: %v", err)
		return &pb.WaitResponse{Success: false, Message: "Failed to acquire lock in time"}, nil
	}
	duration := time.Since(startTime)
	log.WithFields(log.Fields{
		"duration": duration,
		"slot":     in.GetSlotName(),
	}).Info("Acquired slot")
	return &pb.WaitResponse{Success: true, Message: "Waited successfully"}, nil
}

func startGrpcServer(throttler throttler.Throttler, port int) {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
	s := grpc.NewServer()
	healthcheck := health.NewServer()
	healthgrpc.RegisterHealthServer(s, healthcheck)

	service := NewPodLimitersServer(throttler)

	pb.RegisterPodLimiterServer(s, service)

	log.Printf("server listening at %v", lis.Addr())
	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}
