package forkcheckin

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
)

func TestNetworkSOCKS5VaultAuthenticationAndRemoteName(t *testing.T) {
	var siteHits atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		siteHits.Add(1)
		if r.Header.Get("Authorization") != "Bearer site-token" || r.Header.Get("Proxy-Authorization") != "" {
			t.Error("SOCKS credential reached the site, or site session was lost")
		}
		_, _ = io.WriteString(w, "socks")
	}))
	defer origin.Close()
	target, _ := url.Parse(origin.URL)
	port, err := strconv.Atoi(target.Port())
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	var connections sync.Map
	var remoteNames atomic.Int32
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Store(conn, struct{}{})
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer connections.Delete(conn)
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				read := func(size int) []byte {
					value := make([]byte, size)
					if _, err := io.ReadFull(conn, value); err != nil {
						return nil
					}
					return value
				}
				greeting := read(2)
				if len(greeting) != 2 || greeting[0] != 5 || read(int(greeting[1])) == nil {
					return
				}
				_, _ = conn.Write([]byte{5, 2})
				auth := read(2)
				if len(auth) != 2 || auth[0] != 1 {
					return
				}
				user := read(int(auth[1]))
				passwordSize := read(1)
				if len(passwordSize) != 1 {
					return
				}
				password := read(int(passwordSize[0]))
				if string(user) != "proxy-user" || string(password) != "proxy-password" {
					_, _ = conn.Write([]byte{1, 1})
					return
				}
				_, _ = conn.Write([]byte{1, 0})
				connect := read(5)
				if len(connect) != 5 || connect[0] != 5 || connect[1] != 1 || connect[3] != 3 {
					return
				}
				host, requestedPort := read(int(connect[4])), read(2)
				if string(host) != "localhost" || len(requestedPort) != 2 || int(binary.BigEndian.Uint16(requestedPort)) != port {
					return
				}
				remoteNames.Add(1)
				upstream, err := net.DialTimeout("tcp", target.Host, time.Second)
				if err != nil {
					return
				}
				defer upstream.Close()
				_, _ = conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
				done := make(chan struct{})
				go func() {
					_, _ = io.Copy(upstream, conn)
					upstream.Close()
					close(done)
				}()
				_, _ = io.Copy(conn, upstream)
				conn.Close()
				<-done
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		connections.Range(func(key, _ any) bool { key.(net.Conn).Close(); return true })
		workers.Wait()
	})
	factory := networkFactory(t, networkConfig{})
	address := "http://localhost:" + target.Port()
	snapshot := networkSnapshot(t, address, NetworkCredential{Version: 1, Bearer: "site-token", Proxy: &contract.ProxyCredential{Username: "proxy-user", Password: "proxy-password"}})
	snapshot.Account.Network = Network{Mode: NetworkModeCustom, ProxyURL: "socks5://" + listener.Addr().String()}
	client := networkClient(t, factory, snapshot)
	if got := networkGet(t, client, address); got != "socks" {
		t.Fatal("SOCKS5 did not forward the request")
	}
	wrong := networkSnapshot(t, address, NetworkCredential{Version: 1, Bearer: "site-token", Proxy: &contract.ProxyCredential{Username: "proxy-user", Password: "wrong-canary"}})
	wrong.Account.Network = snapshot.Account.Network
	wrong.Account.Revision++
	client = networkClient(t, factory, wrong)
	request, _ := http.NewRequest(http.MethodGet, address, nil)
	if _, err := client.Do(request); !errors.Is(err, ErrNetwork) || strings.Contains(err.Error(), "canary") {
		t.Fatalf("SOCKS rejection was not safe: %v", err)
	}
	if remoteNames.Load() != 1 || siteHits.Load() != 1 {
		t.Fatal("SOCKS authentication failed open or did not preserve remote name resolution")
	}
	factory.Close(context.Background())
}
