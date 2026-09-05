//go:build integration

package main

import (
	"github.com/stretchr/testify/require"
	"os"
	"testing"
)

func TestFlociProcessIntegration(t *testing.T) {
	endpoint := os.Getenv("FLOCI_ENDPOINT")
	require.NotEmpty(t, endpoint)
	runHAProcessIntegration(t, endpoint)
}
