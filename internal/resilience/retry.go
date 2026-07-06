package resilience

import (
	"context"
	"math/rand"
	"time"
)

// Retry executes the operation with exponential backoff and jitter.
func Retry(ctx context.Context, maxAttempts int, baseDelay time.Duration, op func() error) error {
	var err error
	
	// Create a local random generator to avoid global seed issues
	r := rand.New(rand.NewSource(time.Now().UnixNano()))

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err = op()
		if err == nil {
			return nil
		}

		// Don't sleep if it's the last attempt
		if attempt == maxAttempts {
			break
		}

		// Calculate exponential backoff: baseDelay * 2^(attempt-1)
		temp := float64(baseDelay) * float64(int(1)<<(attempt-1))
		
		// Add jitter: random deviation between 50% and 150% of the backoff delay
		jitter := time.Duration(temp * (0.5 + r.Float64()))

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(jitter):
		}
	}
	
	return err
}
