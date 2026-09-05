//go:build windows

package main

import (
	"errors"
	"io"
)

func launchGatewayProcess(string, []string, string, io.Writer) error {
	return errors.New("the Rein Gateway daemon is not yet supported on Windows")
}
