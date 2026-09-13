package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

type fragmentWriter struct {
	bytes.Buffer
}

func (w *fragmentWriter) Write(b []byte) (int, error) {
	if len(b) > 1 {
		b = b[:1]
	}
	return w.Buffer.Write(b)
}

type fragmentReader struct{ io.Reader }

func (r fragmentReader) Read(b []byte) (int, error) {
	if len(b) > 1 {
		b = b[:1]
	}
	return r.Reader.Read(b)
}

type stalledWriter struct{}

func (stalledWriter) Write([]byte) (int, error) { return 0, nil }

type failingWriter struct{}

func (failingWriter) Write(b []byte) (int, error) { return len(b), io.ErrClosedPipe }

func TestFramesFragmentationAndWire(t *testing.T) {
	obj := TunnelConnecData{ID: "id", ServiceName: "web:one", SourceAddress: "127.0.0.1:42"}
	var output fragmentWriter
	if err := WriteObject(&output, obj); err != nil {
		t.Fatal(err)
	}
	want := `{"id":"id","serviceName":"web:one","sourceAddress":"127.0.0.1:42"}`
	if binary.LittleEndian.Uint16(output.Bytes()) != uint16(len(want)) || string(output.Bytes()[2:]) != want {
		t.Fatalf("wire changed: %q", output.Bytes())
	}
	got, err := ReadFrame(fragmentReader{bytes.NewReader(output.Bytes())})
	if err != nil || string(got) != want {
		t.Fatalf("fragmented read: %q, %v", got, err)
	}
}

func TestFrameBounds(t *testing.T) {
	for _, size := range []int{0, 999, 1000, 1001, 65535} {
		t.Run(stringSize(size), func(t *testing.T) {
			b := make([]byte, size+2)
			binary.LittleEndian.PutUint16(b, uint16(size))
			got, err := ReadFrame(bytes.NewReader(b))
			if size <= MaxFrameSize {
				if err != nil || len(got) != size {
					t.Fatalf("size %d: len=%d err=%v", size, len(got), err)
				}
			} else if err == nil {
				t.Fatal("oversize read accepted")
			}
			if size >= 2 {
				var out bytes.Buffer
				err := WriteObject(&out, strings.Repeat("a", size-2))
				if size <= MaxFrameSize {
					if err != nil || out.Len() != size+2 {
						t.Fatalf("size %d: len=%d err=%v", size, out.Len(), err)
					}
				} else if err == nil || out.Len() != 0 {
					t.Fatal("oversize write partially emitted or accepted")
				}
			}
		})
	}
}

func stringSize(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestFrameErrors(t *testing.T) {
	for _, input := range [][]byte{nil, {1}, {2, 0, 'a'}} {
		if _, err := ReadFrame(bytes.NewReader(input)); err == nil {
			t.Fatalf("truncated frame accepted: %v", input)
		}
	}
	if err := WriteObject(stalledWriter{}, "test"); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("stalled writer: %v", err)
	}
	if err := WriteObject(failingWriter{}, "test"); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("full-count write error lost: %v", err)
	}
	if err := WriteObject(io.Discard, make(chan int)); err == nil {
		t.Fatal("marshal error lost")
	}
}

func TestRegistrationWireCompatibility(t *testing.T) {
	b, err := json.Marshal(TunnelData{})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"","serviceName":"","token":"","allowedSources":"","frontendData":{"port":0,"tlsWrap":false,"sshWrap":false},"BackendAcceptBacklog":0,"TargetPort":0,"TargetAddresses":null}`
	if string(b) != want {
		t.Fatalf("registration wire changed: %s", b)
	}
	b, _ = json.Marshal(TunnelDataResponse{})
	if bytes.Contains(b, []byte("publicationMode")) {
		t.Fatal("empty additive response field must be omitted")
	}
}
