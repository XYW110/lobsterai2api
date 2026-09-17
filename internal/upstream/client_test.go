package upstream

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lobsterai2api/internal/auth"
)

// newTestClient 指向上游测试服务器，返回隔离的 Client。
func newTestClient(t *testing.T, ts *httptest.Server) *Client {
	t.Helper()
	prev := ServerBase()
	SetServerBase(ts.URL)
	t.Cleanup(func() { SetServerBase(prev) })
	return New()
}

// TestChatStreamPassthroughByteExact 正常 200 流必须字节级无损：
// 窥探消耗的流首要原样交给下游（覆盖 > peekBufSize 的多帧流）。
func TestChatStreamPassthroughByteExact(t *testing.T) {
	var want bytes.Buffer
	want.WriteString("data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
	for i := 0; i < 300; i++ {
		// 每帧 ~200B，300 帧 ≈ 60KB > peekBufSize(4KB)
		fmt.Fprintf(&want, "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"chunk-%04d-padding-%s\"}}]}\n\n",
			i, strings.Repeat("x", 150))
	}
	want.WriteString("data: [DONE]\n\n")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(want.Bytes())
	}))
	defer ts.Close()

	c := newTestClient(t, ts)
	rc, status, err := c.ChatStream(&auth.Auth{AccessToken: "tok", UID: "u1"}, []byte(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatalf("ChatStream transport error: %v", err)
	}
	if status != http.StatusOK || rc == nil {
		t.Fatalf("expected rc!=nil status=200, got rc=%v status=%d", rc, status)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("stream not byte-exact: got %d bytes want %d bytes", len(got), want.Len())
	}
}

// TestChatStreamErrorFrameIn200 HTTP 200 流首携带业务错误帧必须被拦截：
// rc==nil、status 仍为 200、LastBody 可分类为 hard_credit。
func TestChatStreamErrorFrameIn200(t *testing.T) {
	errFrame := "event:error\n" +
		"data: {\"type\":\"error\",\"error\":{\"code\":40201,\"message\":\"免费额度已用完，请升级套餐\"}}\n\n"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(errFrame))
	}))
	defer ts.Close()

	c := newTestClient(t, ts)
	rc, status, err := c.ChatStream(&auth.Auth{AccessToken: "tok", UID: "u1"}, []byte(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatalf("ChatStream transport error: %v", err)
	}
	if rc != nil {
		rc.Close()
		t.Fatalf("expected rc=nil for in-stream error frame, got rc!=nil (status=%d)", status)
	}
	if status != http.StatusOK {
		t.Fatalf("expected status=200 (frame hidden in 200), got %d", status)
	}
	if !strings.Contains(string(c.LastBody), "40201") {
		t.Fatalf("LastBody should carry the error frame, got: %s", truncate(string(c.LastBody), 200))
	}
	if kind := Classify(status, string(c.LastBody)); kind != ErrHardCredit {
		t.Fatalf("Classify should be hard_credit, got %s", kind)
	}
}
