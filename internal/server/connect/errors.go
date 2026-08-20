package connect

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/treeol/wakil/internal/core"
)

// mapError converts a core sentinel error to a Connect error code.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, core.ErrSessionNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, core.ErrSessionClosed):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, core.ErrSessionBusy):
		return connect.NewError(connect.CodeResourceExhausted, err)
	case errors.Is(err, core.ErrNotAuthorized):
		return connect.NewError(connect.CodePermissionDenied, err)
	case errors.Is(err, core.ErrInvalidInput):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, core.ErrInvalidStateTransition):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, core.ErrApprovalNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, core.ErrApprovalAlreadyResolved):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, err)
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}
