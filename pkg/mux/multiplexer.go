package mux

import (
	"bytes"
	"errors"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type MultiplexerConfig struct {
	SSH  bool
	HTTP bool
}

type Multiplexer struct {
	sync.RWMutex
	protocols      map[string]*multiplexerListener
	done           bool
	listeners      map[string]net.Listener
	newConnections chan net.Conn
}

func (m *Multiplexer) StartListener(network, address string) error {
	m.Lock()
	defer m.Unlock()

	if _, ok := m.listeners[address]; ok {
		return errors.New("Address " + address + " already listening")
	}

	listener, err := net.Listen(network, address)
	if err != nil {
		return err
	}

	m.listeners[address] = listener

	go func(listen net.Listener) {
		for {
			conn, err := listen.Accept()
			if err != nil {
				if strings.Contains(err.Error(), "use of closed network connection") {
					m.Lock()

					delete(m.listeners, address)

					m.Unlock()
					return
				}
				continue

			}

			go func() {
				m.newConnections <- conn
			}()
		}

	}(listener)

	return nil
}

func (m *Multiplexer) StopListener(address string) error {
	m.Lock()
	defer m.Unlock()

	listener, ok := m.listeners[address]
	if !ok {
		return errors.New("Address " + address + " not listening")
	}

	return listener.Close()
}

func (m *Multiplexer) GetListeners() []string {
	m.RLock()
	defer m.RUnlock()

	listeners := []string{}
	for l := range m.listeners {
		listeners = append(listeners, l)
	}

	sort.Strings(listeners)

	return listeners
}

func ListenWithConfig(network, address string, c MultiplexerConfig) (*Multiplexer, error) {

	var m Multiplexer

	m.newConnections = make(chan net.Conn)
	m.listeners = make(map[string]net.Listener)
	m.protocols = map[string]*multiplexerListener{}

	err := m.StartListener(network, address)
	if err != nil {
		return nil, err
	}

	if c.SSH {
		m.protocols["ssh"] = newMultiplexerListener(m.listeners[address].Addr())
	}

	if c.HTTP {
		m.protocols["http"] = newMultiplexerListener(m.listeners[address].Addr())
	}

	var waitingConnections int32
	go func() {
		for conn := range m.newConnections {

			if atomic.LoadInt32(&waitingConnections) > 1000 {
				conn.Close()
				continue
			}

			//Atomic as other threads may be writing and reading while we do this
			atomic.AddInt32(&waitingConnections, 1)
			go func(conn net.Conn) {

				conn.SetDeadline(time.Now().Add(2 * time.Second))
				l, prefix, err := m.determineProtocol(conn)
				if err != nil {
					conn.Close()
					log.Println("Multiplexing failed: ", err)
					return
				}

				conn.SetDeadline(time.Time{})

				select {
				//Allow whatever we're multiplexing to apply backpressure if we cant accept things
				case l.connections <- &bufferedConn{conn: conn, prefix: prefix}:
				case <-time.After(2 * time.Second):
					conn.Close()
				}

				atomic.AddInt32(&waitingConnections, -1)
			}(conn)

		}
	}()

	return &m, nil
}

func Listen(network, address string) (*Multiplexer, error) {
	c := MultiplexerConfig{
		SSH:  true,
		HTTP: true,
	}

	return ListenWithConfig(network, address, c)
}

func (m *Multiplexer) Close() {
	m.done = true

	for address := range m.listeners {
		m.StopListener(address)
	}

	for _, v := range m.protocols {
		v.Close()
	}

	close(m.newConnections)

}

func isHttp(b []byte) bool {

	validMethods := [][]byte{
		[]byte("GET"), []byte("HEAD"), []byte("POST"),
		[]byte("PUT"), []byte("DELETE"), []byte("CONNECT"),
		[]byte("OPTIONS"), []byte("TRACE"), []byte("PATCH"),
	}

	for _, vm := range validMethods {
		if bytes.HasPrefix(b, vm) {
			return true
		}
	}

	return false
}

func (m *Multiplexer) determineProtocol(c net.Conn) (*multiplexerListener, []byte, error) {
	b := make([]byte, 3)
	_, err := c.Read(b)
	if err != nil {
		return nil, nil, err
	}

	proto := ""
	if bytes.HasPrefix(b, []byte{'S', 'S', 'H'}) {
		proto = "ssh"
	} else if isHttp(b) {
		proto = "http"
	}

	l, ok := m.protocols[proto]
	if !ok {
		return nil, nil, errors.New("Unknown protocol")
	}

	return l, b, nil
}

func (m *Multiplexer) getProtoListener(proto string) net.Listener {
	ml, ok := m.protocols[proto]
	if !ok {
		panic("Unknown protocol passed: " + proto)
	}

	return ml
}

func (m *Multiplexer) SSH() net.Listener {
	return m.getProtoListener("ssh")
}

func (m *Multiplexer) HTTP() net.Listener {
	return m.getProtoListener("http")
}
