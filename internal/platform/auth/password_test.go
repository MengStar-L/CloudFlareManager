package auth

import "testing"

func TestPasswordHashAndVerify(t *testing.T) {
	t.Parallel()

	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(hash, "correct horse battery staple") {
		t.Fatal("password should verify")
	}
	if VerifyPassword(hash, "wrong") {
		t.Fatal("wrong password verified")
	}
}

func TestPasswordRejectsShortValues(t *testing.T) {
	t.Parallel()

	if _, err := HashPassword("short"); err == nil {
		t.Fatal("expected short password error")
	}
}
