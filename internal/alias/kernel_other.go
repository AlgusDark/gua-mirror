//go:build !linux

package alias

import (
	"errors"
	"net"
)

// newKernelOps on non-Linux platforms returns a stub that errors from
// every operation. The daemon's Dockerfile builds for linux/$TARGETARCH
// so this code path is only reached if a contributor runs a host-native
// binary on macOS or Windows. Build/vet succeed; runtime fails with a
// clear message.
func newKernelOps() kernelOps {
	return unsupportedOps{}
}

type unsupportedOps struct{}

var errUnsupported = errors.New(
	"alias: gua-mirror only runs on Linux (this build has no netlink support)")

func (unsupportedOps) listDeprecatedV6(string) ([]net.IP, error) { return nil, errUnsupported }
func (unsupportedOps) addDeprecatedV6(string, net.IP) error      { return errUnsupported }
func (unsupportedOps) deleteV6(string, net.IP) error             { return errUnsupported }
