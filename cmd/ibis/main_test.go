package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestErrMessageMasksRPCURL(t *testing.T) {
	err := fmt.Errorf("engine: %w", errors.New(`Post "https://node.example/rpc/SECRETTOKEN": EOF`))
	got := errMessage(err)
	if strings.Contains(got, "SECRETTOKEN") || !strings.Contains(got, "node.example") {
		t.Errorf("not masked: %s", got)
	}
}
