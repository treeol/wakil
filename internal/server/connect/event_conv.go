package connect

import (
	v1alpha1 "github.com/treeol/wakil/api/gen/wakil/v1alpha1"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/protoconv"
)

// eventToProto converts a core.Event to a proto Event.
func eventToProto(e event.Event) (*v1alpha1.Event, error) {
	return protoconv.EventToProto(e)
}

// eventFromProto converts a proto Event to a core.Event.
func eventFromProto(pb *v1alpha1.Event) (event.Event, error) {
	return protoconv.EventFromProto(pb)
}
