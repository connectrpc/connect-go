package rerpc

import (
	"context"
	"errors"
	"testing"

	"github.com/akshayjshah/rerpc/internal/assert"
)

func TestHooks(t *testing.T) {
	ctx := context.Background()
	err := errors.New("oh no")

	t.Run("nil", func(t *testing.T) {
		var h *Hooks
		h.onInternalError(ctx, err)
		h.onNetworkError(ctx, err)
		h.onMarshalError(ctx, err)
	})

	t.Run("zero", func(t *testing.T) {
		var h Hooks
		h.onInternalError(ctx, err)
		h.onNetworkError(ctx, err)
		h.onMarshalError(ctx, err)
	})

	t.Run("nonzero", func(t *testing.T) {
		var calls int
		increment := func(_ context.Context, _ error) {
			calls++
		}
		h := &Hooks{
			OnInternalError: increment,
			OnNetworkError:  increment,
			OnMarshalError:  increment,
		}
		h.onInternalError(ctx, err)
		h.onNetworkError(ctx, err)
		h.onMarshalError(ctx, err)
		assert.Equal(t, calls, 3, "expected one call per error")
	})
}
