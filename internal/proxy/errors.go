package proxy

import (
	"errors"
	"net/http"
)

// routeError ferries an HTTP status + user-facing message out of the route-
// resolution helpers so the dispatching handler can write a consistent
// response without branching on error types directly.
type routeError struct {
	status  int
	message string
}

func (e routeError) Error() string {
	return e.message
}

func writeRouteError(w http.ResponseWriter, err error) {
	var routeErr routeError
	if errors.As(err, &routeErr) {
		http.Error(w, routeErr.message, routeErr.status)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}
