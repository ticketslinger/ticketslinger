package auth

import "testing"

func TestPasswordRoundTrip(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := CheckPassword(h, "correct horse battery"); !ok {
		t.Fatal("right password rejected")
	}
	if ok, _ := CheckPassword(h, "wrong horse battery"); ok {
		t.Fatal("wrong password accepted")
	}
}

func TestHashesAreSalted(t *testing.T) {
	a, _ := HashPassword("same password here")
	b, _ := HashPassword("same password here")
	if a == b {
		t.Fatal("two hashes of one password must differ")
	}
}
