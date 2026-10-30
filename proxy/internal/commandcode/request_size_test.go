package commandcode

import "testing"

func TestRequestTooLargeClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  APIError
		want bool
	}{
		{"HTTP status", APIError{Status: 413}, true},
		{"code", APIError{Status: 400, Code: "REQUEST_TOO_LARGE"}, true},
		{"provider type", APIError{Type: "request_too_large"}, true},
		{"embedded signature", APIError{Message: `{"error":{"type":"request_too_large"}}`}, true},
		{"request limit", APIError{Message: "Over the 32 MB limit for one request"}, true},
		{"body limit", APIError{Message: "Request body exceeds the 20 MB limit"}, true},
		{"single image limit", APIError{Status: 400, Message: "over the 5 MB limit for one image"}, false},
		{"context window", APIError{Status: 400, Message: "prompt is too long"}, false},
		{"worker limit", APIError{Status: 500, Message: "worker_exceeded_resources: error 1102"}, false},
		{"generic resource limit", APIError{Message: "exceeded its resource limits"}, false},
		{"rate limit", APIError{Status: 429, Message: "Too many requests"}, false},
		{"auth", APIError{Status: 403, Type: "invalid_api_key"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.IsRequestTooLarge(); got != tc.want {
				t.Fatalf("IsRequestTooLarge() = %t, want %t", got, tc.want)
			}
		})
	}
}
