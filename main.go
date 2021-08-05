package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/joshuarubin/lifecycle"
)

func main() {
	ctx := lifecycle.New(context.Background())
	if err := run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		os.Exit(1)
	}
}

type State struct {
	id     int
	rig    rig
	conn   net.Conn
	freq   string
	mode   string
	ignore *time.Timer

	mu sync.Mutex
}

type rig struct {
	addr          string
	buggyPassband bool
	cwFlipped     bool
	noPktSB       bool
}

var (
	rigs = []rig{
		{addr: "[::1]:4542", buggyPassband: true},
		{addr: "127.0.0.1:7356", cwFlipped: true, noPktSB: true},
	}
	states = make([]*State, len(rigs))
)

func run(ctx context.Context) error {
	for i, rig := range rigs {
		states[i] = &State{
			id:  i,
			rig: rig,
		}
		if _, err := states[i].fetch(); err != nil {
			return err
		}
	}

	for _, state := range states {
		lifecycle.GoCtxErr(ctx, state.loop)
	}

	return lifecycle.Wait(ctx)
}

func (s *State) clear() {
	s.conn = nil
	s.freq = ""
	s.mode = ""
	s.ignoreFor(0, false)
}

func (s *State) getConn() net.Conn {
	if s.conn != nil {
		return s.conn
	}

	conn, err := net.Dial("tcp", s.rig.addr)
	if err != nil {
		return nil
	}

	s.conn = conn

	fmt.Fprintf(os.Stderr, "connected to %s\n", s.rig.addr)

	if _, err = s.fetch(); err != nil {
		fmt.Fprintf(os.Stderr, "error initializing connection %s: %s\n", s.rig.addr, err)
	}

	var cloneFrom *State
	if s.id > 0 && len(states) > 0 && states[0] != nil {
		cloneFrom = states[0]
	} else if s.id == 0 {
		for i := 1; i < len(states); i++ {
			state := states[i]
			if state != nil && state.getConn() != nil {
				cloneFrom = state
				break
			}
		}
	}

	if cloneFrom != nil {
		if err = cloneFrom.cloneTo(s.id); err != nil {
			fmt.Fprintf(os.Stderr, "error initializing connection %s: %s\n", s.rig.addr, err)
		}
	}

	return conn
}

func (s *State) fetch() (bool, error) {
	diffF, err := s.getFrequency()
	if err != nil {
		return false, err
	}

	diff := diffF

	diffM, err := s.getMode()
	if err != nil {
		return false, err
	}

	diff = diff || diffM

	if s.ignoring() && diff {
		fmt.Printf("not cloning change from %s\n", s.rig.addr)
		return false, nil
	}

	return diff, nil
}

func (s *State) getFrequency() (bool, error) {
	return s.getString("f", &s.freq)
}

func (s *State) getMode() (bool, error) {
	return s.getString("m", &s.mode)
}

func flipCW(val string) string {
	switch {
	case val[:3] == "CW ":
		return "CWR " + val[3:]
	case val[:4] == "CWR ":
		return "CW " + val[4:]
	}
	return val
}

func (s *State) getString(cmd string, dest *string) (bool, error) {
	val, err := s.sendCmd(cmd)
	if errors.Is(err, io.EOF) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	// work around buggy mode reports. act like the passband is 0 no matter what
	// was received.
	if cmd == "m" && s.rig.buggyPassband {
		val = val[0:strings.Index(val, " ")]
		val += " 0"
	}

	if cmd == "m" && strings.HasPrefix(val, "CW") && s.rig.cwFlipped {
		val = flipCW(val)
	}

	diff := *dest != val
	*dest = val
	if diff {
		fmt.Printf("got %s from %s: %q\n", cmd, s.rig.addr, val)
	}
	return diff, nil
}

func (s *State) setFrequency(freq string) error {
	return s.setString("F", freq, &s.freq)
}

func (s *State) setMode(mode string) error {
	// work around buggy mode reports. act like the passband is 0 no matter what
	// was received.
	if s.rig.buggyPassband {
		mode = mode[0:strings.Index(mode, " ")]
		mode += " 0"
	}

	if strings.HasPrefix(mode, "CW") && s.rig.cwFlipped {
		mode = flipCW(mode)
	}

	if (strings.HasPrefix(mode, "PKTUSB") || strings.HasPrefix(mode, "PKTLSB")) && s.rig.noPktSB {
		mode = mode[3:]
	}

	return s.setString("M", mode, &s.mode)
}

func (s *State) setString(cmd, val string, dest *string) error {
	if *dest == val {
		return nil
	}

	fmt.Printf("setting %s on %s to %s\n", cmd, s.rig.addr, val)
	if _, err := s.sendCmd(cmd, val); err != nil {
		return err
	}
	*dest = val
	return nil
}

func (s *State) ignoreFor(val time.Duration, lock bool) {
	if lock {
		s.mu.Lock()
		defer s.mu.Unlock()
	}

	if s.ignore != nil {
		if val > 0 {
			s.ignore.Reset(val)
		} else {
			s.ignore.Stop()
			s.ignore = nil
		}
		return
	}

	if val == 0 {
		return
	}

	ignore := time.NewTimer(val)
	s.ignore = ignore

	go func() {
		<-ignore.C
		s.mu.Lock()
		if ignore == s.ignore {
			s.ignore = nil
		}
		s.mu.Unlock()
	}()
}

func (s *State) ignoring() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ignore != nil
}

func (s *State) cloneTo(destID int) error {
	if s.id == destID {
		return nil
	}

	state := states[destID]

	if state.getConn() == nil {
		return nil
	}

	fmt.Printf("cloning from %s to %s\n", s.rig.addr, state.rig.addr)

	state.ignoreFor(200*time.Millisecond, true)

	if err := state.setFrequency(s.freq); err != nil {
		return err
	}

	if err := state.setMode(s.mode); err != nil {
		return err
	}

	return nil
}

func (s *State) cloneToAll() error {
	for id := range states {
		if err := s.cloneTo(id); err != nil {
			return err
		}
	}
	return nil
}

func (s *State) sendCmd(cmd ...string) (string, error) {
	conn := s.getConn()
	if conn == nil {
		return "", io.EOF
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := conn.Write([]byte(strings.Join(cmd, " ") + "\n")); err != nil {
		return "", fmt.Errorf("error writing to %s: %w", s.rig.addr, err)
	}

	buf := make([]byte, 128)
	var str string
	for {
		n, err := conn.Read(buf)
		if err != nil {
			var oerr *net.OpError
			if (errors.As(err, &oerr) && oerr.Op == "read" && oerr.Net == "tcp") || err == io.EOF {
				fmt.Println("disconnected from:", s.rig.addr)
				s.clear()
				return "", io.EOF
			}

			return "", fmt.Errorf("error reading from %s: %w", s.rig.addr, err)
		}
		str += string(buf[:n])
		if strings.HasSuffix(str, "\n") {
			break
		}
	}
	str = strings.ReplaceAll(str, "\n", " ")
	str = strings.TrimSpace(str)
	return str, nil
}

func (s *State) loop(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			diff, err := s.fetch()
			if err != nil {
				return err
			}

			if diff {
				s.cloneToAll()
			}
		}
	}
}
