package main

import (
	"context"
	"errors"

	"berth/spike/go/gen/spike/v1/spikev1connect"
)

type screencaster struct{}

func startScreencaster(context.Context) (*screencaster, error) {
	return nil, errors.New("not built yet")
}
func (s *screencaster) close() {}
func (s *screencaster) watch(context.Context, spikev1connect.SpikeServiceWatchScreenServerStream) error {
	return nil
}
