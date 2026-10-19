package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"yoakeland/internal/protocol"
	"yoakeland/internal/sockpath"
)

// runPair is `yoakeland pair`: it asks the running server, over its local socket, for a fresh
// one-device pairing code and prints it. A server run as a service never prints one at startup
// (that needs a terminal), so this is how its owner pairs a device. Full access by default,
// since whoever can run this on the server owns it; -phone makes a limited one.
func runPair(args []string) int {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	stateDir := fs.String("state-dir", sockpath.DefaultStateDir(), "the running server's -state-dir")
	socket := fs.String("socket", "", "the server's local socket, if it was started with -listen unix:<path>")
	host := fs.String("host", "", "address clients reach this server on (default: the address you ssh'd to, else a LAN address)")
	phone := fs.Bool("phone", false, "make a limited code for a phone instead of a full-access one")
	_ = fs.Parse(args)

	path := *socket
	if path == "" {
		path = sockpath.DefaultSocketPath(sockpath.DefaultRuntimeDir(*stateDir))
	}
	conn, err := sockpath.Dial(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "can't reach the server at %s: %v\n(is it running? pass -state-dir or -socket if it isn't using the defaults)\n", path, err)
		return 1
	}
	defer conn.Close()

	id, err := generateAuthToken()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	name := "pair-" + id[:6]
	data, _ := json.Marshal(protocol.RegisterTokenData{Name: name, Pairing: true, Full: !*phone})
	req, _ := json.Marshal(protocol.Envelope{Type: protocol.EventRegisterToken, Data: data})
	fmt.Fprintf(conn, "%s\n", req)

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	port := 0
	var reg protocol.TokenRegisteredData
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 1<<20), protocol.MaxFrameSize)
	for sc.Scan() {
		var env protocol.Envelope
		if json.Unmarshal(sc.Bytes(), &env) != nil {
			continue
		}
		switch env.Type {
		case protocol.EventRemoteAccessSet:
			var d protocol.RemoteAccessSetData
			_ = json.Unmarshal(env.Data, &d)
			if i := strings.LastIndex(d.Addr, ":"); d.Enabled && i >= 0 {
				port, _ = strconv.Atoi(d.Addr[i+1:])
			}
		case protocol.EventError:
			var d protocol.ErrorData
			_ = json.Unmarshal(env.Data, &d)
			fmt.Fprintln(os.Stderr, "server refused:", d.Message)
			return 1
		case protocol.EventTokenRegistered:
			_ = json.Unmarshal(env.Data, &reg)
		}
		if reg.Token != "" && port != 0 {
			break
		}
	}
	if reg.Token == "" {
		fmt.Fprintln(os.Stderr, "the server didn't answer")
		return 1
	}
	if port == 0 {
		fmt.Fprintln(os.Stderr, "remote access is off on this server: turn it on (set_remote_access, or start with -listen tcp) and run this again")
		return 1
	}
	hosts := pairHosts()
	if *host != "" {
		hosts = []string{*host}
	}
	if len(hosts) == 0 {
		fmt.Fprintln(os.Stderr, "can't tell this server's address: pass -host")
		return 1
	}
	tier := "full access"
	if *phone {
		tier = "limited (phone)"
	}
	fmt.Fprintf(os.Stderr, "Pairing code for %s:%d: %s, valid for %d minutes, one device.\nPaste into the app's Pairing code field:\n", hosts[0], port, tier, int(PairingTTL.Minutes()))
	fmt.Println(pairingCode(hosts, port, reg.Token, reg.RTCFingerprint, reg.CaPem))
	return 0
}
