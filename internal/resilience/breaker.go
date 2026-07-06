package resilience

import (
	"errors"
	"sync"
	"time"
)

// State defines the current status of the Circuit Breaker.
type State int

const (
	StateClosed State = iota
	StateOpen
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "CLOSED"
	case StateOpen:
		return "OPEN"
	case StateHalfOpen:
		return "HALF-OPEN"
	default:
		return "UNKNOWN"
	}
}

var ErrCircuitOpen = errors.New("circuit breaker is open")

// CircuitBreaker protects calls to unhealthy upstream services.
type CircuitBreaker struct {
	mu                   sync.RWMutex
	state                State
	consecutiveFailures  int
	consecutiveSuccesses int
	failureThreshold     int
	successThreshold     int
	cooldown             time.Duration
	lastStateChange      time.Time
}

// NewCircuitBreaker creates a circuit breaker with specified limits.
func NewCircuitBreaker(failureThreshold, successThreshold int, cooldown time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		state:            StateClosed,
		failureThreshold: failureThreshold,
		successThreshold: successThreshold,
		cooldown:         cooldown,
		lastStateChange:  time.Now(),
	}
}

// Execute wraps an operation with circuit breaker state management.
func (cb *CircuitBreaker) Execute(op func() error) error {
	if !cb.allowRequest() {
		return ErrCircuitOpen
	}

	err := op()

	cb.recordResult(err)
	return err
}

func (cb *CircuitBreaker) allowRequest() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.state == StateOpen {
		// Check if cooldown period has elapsed
		if time.Since(cb.lastStateChange) > cb.cooldown {
			cb.state = StateHalfOpen
			cb.consecutiveSuccesses = 0
			cb.lastStateChange = time.Now()
			return true
		}
		return false
	}

	return true
}

func (cb *CircuitBreaker) recordResult(err error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if err != nil {
		cb.consecutiveFailures++
		cb.consecutiveSuccesses = 0

		if cb.state == StateClosed && cb.consecutiveFailures >= cb.failureThreshold {
			cb.state = StateOpen
			cb.lastStateChange = time.Now()
		} else if cb.state == StateHalfOpen {
			// Fail immediately back to open on any failure in half-open
			cb.state = StateOpen
			cb.lastStateChange = time.Now()
		}
	} else {
		// Success
		cb.consecutiveFailures = 0

		if cb.state == StateHalfOpen {
			cb.consecutiveSuccesses++
			if cb.consecutiveSuccesses >= cb.successThreshold {
				cb.state = StateClosed
				cb.consecutiveSuccesses = 0
			}
		}
	}
}

// State returns the current state of the circuit breaker.
func (cb *CircuitBreaker) State() State {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state
}
