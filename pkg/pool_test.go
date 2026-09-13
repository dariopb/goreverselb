package tunnel

import "testing"

func TestPoolIntsAllocationAndReturn(t *testing.T) {
	pool := NewPoolForRange(8000, 2)
	if port, err := pool.GetElement(8000); err != nil || port != 8000 {
		t.Fatalf("explicit allocation got port %d and error %v", port, err)
	}
	if _, err := pool.GetElement(8000); err == nil {
		t.Fatal("duplicate explicit allocation unexpectedly succeeded")
	}
	port, err := pool.GetElement(0)
	if err != nil || port != 8001 {
		t.Fatalf("dynamic allocation got port %d and error %v", port, err)
	}
	if _, err := pool.GetElement(0); err == nil {
		t.Fatal("allocation from exhausted pool unexpectedly succeeded")
	}
	if err := pool.ReturnElement(8000); err != nil {
		t.Fatal(err)
	}
	if port, err := pool.GetElement(8000); err != nil || port != 8000 {
		t.Fatalf("returned port allocation got port %d and error %v", port, err)
	}
}
