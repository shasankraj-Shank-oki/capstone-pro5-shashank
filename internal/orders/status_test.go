package orders

import "testing"

func TestValidStatus(t *testing.T) {
	for _, status := range []Status{StatusCreated, StatusConfirmed, StatusCancelled} {
		if !ValidStatus(status) {
			t.Errorf("ValidStatus(%q) = false, want true", status)
		}
	}

	for _, status := range []Status{"", "PENDING", "created"} {
		if ValidStatus(status) {
			t.Errorf("ValidStatus(%q) = true, want false", status)
		}
	}
}
