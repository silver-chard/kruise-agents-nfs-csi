package wrapper

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/silver-chard/kruise-agents-nfs-csi/internal/node"
)

func TestRequestErrorStatus(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "bad request", err: errBadRequest, want: http.StatusBadRequest},
		{name: "bad source subpath", err: node.ErrBadSourceSubPath, want: http.StatusBadRequest},
		{name: "mount disabled", err: node.ErrMountDisabled, want: http.StatusServiceUnavailable},
		{name: "node operation", err: errNodeOperation, want: http.StatusInternalServerError},
		{name: "authorization", err: errors.New("not authorized"), want: http.StatusForbidden},
		{
			name: "bad source remains a client error when wrapped as a node operation",
			err:  errors.Join(errNodeOperation, fmt.Errorf("open source: %w", node.ErrBadSourceSubPath)),
			want: http.StatusBadRequest,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := requestErrorStatus(test.err); got != test.want {
				t.Fatalf("requestErrorStatus() = %d, want %d", got, test.want)
			}
		})
	}
}
