package kube

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestTokenReviewResponseDecodesBoundPodExtra(t *testing.T) {
	payload := []byte(`{
		"status": {
			"authenticated": true,
			"user": {
				"username": "system:serviceaccount:sandbox-ns:sandbox-sa",
				"extra": {
					"authentication.kubernetes.io/pod-name": ["pod-a"],
					"authentication.kubernetes.io/pod-uid": ["pod-uid"]
				}
			}
		}
	}`)

	var response TokenReviewResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatalf("decode TokenReview response: %v", err)
	}
	want := map[string][]string{
		"authentication.kubernetes.io/pod-name": {"pod-a"},
		"authentication.kubernetes.io/pod-uid":  {"pod-uid"},
	}
	if !reflect.DeepEqual(response.Status.User.Extra, want) {
		t.Fatalf("TokenReview user extra = %#v, want %#v", response.Status.User.Extra, want)
	}
}
