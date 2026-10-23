package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

func checkDestination(
	ctx context.Context,
	rawURL string,
) error {
	ctx, span := tracer.Start(
		ctx,
		"http.verify_destination",
	)
	defer span.End()

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		rawURL,
		nil,
	)
	if err != nil {
		return fmt.Errorf("create destination request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("destination unreachable: %w", err)
	}
	defer resp.Body.Close()

	_, _ = io.ReadAll(resp.Body)

	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf(
			"destination returned status %d",
			resp.StatusCode,
		)
	}

	return nil
}