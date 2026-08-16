package auth

import "context"

// AuthenticatedDevice is who is making a request, resolved once by the middleware and carried
// through to the handler.
//
// Every handler below the auth middleware reads AccountID from here and from nowhere else. A
// handler that took an account id from the request body instead would let any authenticated device
// address another account's data, which is the single most common way a multi-tenant server leaks.
type AuthenticatedDevice struct {
	DeviceID  string
	AccountID string
}

// contextKeyType is unexported so no other package can collide with this key, which is why a bare
// string is not used.
type contextKeyType struct{}

var deviceContextKey contextKeyType

// WithAuthenticatedDevice returns a context carrying the caller's identity.
func WithAuthenticatedDevice(parent context.Context, device AuthenticatedDevice) context.Context {
	return context.WithValue(parent, deviceContextKey, device)
}

// DeviceFrom returns the authenticated caller, and false if the context never passed through the
// auth middleware — which for a route that expected it is a routing bug, not a client error.
func DeviceFrom(ctx context.Context) (AuthenticatedDevice, bool) {
	device, ok := ctx.Value(deviceContextKey).(AuthenticatedDevice)
	return device, ok
}
