package cpace

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

// Every expected value below is copied from draft-irtf-cfrg-cpace-21, appendices A and B.1.

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestStringUtilities(t *testing.T) {
	if got := hex.EncodeToString(PrependLen(nil)); got != "00" {
		t.Errorf("prepend_len(b\"\") = %s", got)
	}
	if got := hex.EncodeToString(LVCat([]byte("1234"), []byte("5"), nil, []byte("678"))); got != "043132333401350003363738" {
		t.Errorf("lv_cat = %s", got)
	}
	// LEB128 continues past 127 bytes.
	if got := PrependLen(make([]byte, 200)); got[0] != 0xc8 || got[1] != 0x01 || len(got) != 202 {
		t.Errorf("prepend_len(200 bytes) starts % x", got[:3])
	}
}

var (
	vecPRS = []byte("Password")
	vecCI  = []byte("\x0bA_initiator\x0bB_responder")
)

const (
	vecSID = "7e4b4791d6a8ef019b936c79fb7f2c57"
	vecG   = "d04bf6d41f6a289632a2e929fa29bebd51092512a7829fdde7d314b62f05a73f"
	vecYa  = "1d13c89278cdadd826f6d8d7f887701430f8380ddc17611cdd6dc989ce0c9f32"
	vecYb  = "248cccf6d5cdc3646f0ad593f9e6cef4e69d4945f8372e623512ecea32185623"
	vecYaS = "21b4f4bd9e64ed355c3eb676a28ebedaf6d8f17bdc365995b319097153044080"
	vecYbS = "848b0779ff415f0af4ea14df9dd1d3c29ac41d836c7808896c4eba19c51ac40a"
	vecK   = "5b067effbdc0b2a0e1d907b21ebb25cfedb96a852179a847c37e43ee71322c6b"
	vecISK = "6e19b875f7a561d6b3ca3dbb9ef42ac55de3e717881018204b8922b4d5e53bb2" +
		"aa82c300bea7b65d2b671da71922ddf6472301b79bc270adfa8bf413285f2263"
)

func TestGeneratorString(t *testing.T) {
	want := unhex(t, `0843506163653235350850617373776f72646d000000000000000000
		00000000000000000000000000000000000000000000000000000000
		00000000000000000000000000000000000000000000000000000000
		00000000000000000000000000000000000000000000000000000000
		00000000000000000000000000000000180b415f696e69746961746f
		720b425f726573706f6e646572107e4b4791d6a8ef019b936c79fb7f
		2c57`)
	if got := generatorString(vecPRS, vecCI, unhex(t, vecSID)); !bytes.Equal(got, want) {
		t.Fatalf("generator string\n got %x\nwant %x", got, want)
	}
}

func TestGeneratorMatchesDraft(t *testing.T) {
	if got := calculateGenerator(vecPRS, vecCI, unhex(t, vecSID)); hex.EncodeToString(got) != vecG {
		t.Fatalf("generator g = %x, want %s", got, vecG)
	}
}

func TestMessagesAndISKMatchDraft(t *testing.T) {
	sid := unhex(t, vecSID)
	a, err := start(vecPRS, vecCI, sid, []byte("ADa"), unhex(t, vecYaS))
	if err != nil {
		t.Fatal(err)
	}
	b, err := start(vecPRS, vecCI, sid, []byte("ADb"), unhex(t, vecYbS))
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(a.Message()) != vecYa || hex.EncodeToString(b.Message()) != vecYb {
		t.Fatalf("Ya %x Yb %x", a.Message(), b.Message())
	}
	for _, s := range []struct {
		who, y string
		scalar string
	}{{"A", vecYb, vecYaS}, {"B", vecYa, vecYbS}} {
		k, err := scalarMultVfy(unhex(t, s.scalar), unhex(t, s.y))
		if err != nil || hex.EncodeToString(k) != vecK {
			t.Fatalf("K as %s = %x, %v; want %s", s.who, k, err, vecK)
		}
	}
	iskA, err := a.FinishInitiator(b.Message(), []byte("ADb"))
	if err != nil {
		t.Fatal(err)
	}
	iskB, err := b.FinishResponder(a.Message(), []byte("ADa"))
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(iskA) != vecISK || !bytes.Equal(iskA, iskB) {
		t.Fatalf("ISK A %x\nISK B %x\nwant  %s", iskA, iskB, vecISK)
	}
}

// B.1.10: low-order points must give the neutral element (an abort), and the others the listed
// results, including u values with bit 255 set, which RFC 7748 says to clear.
func TestScalarMultVfyLowOrderPoints(t *testing.T) {
	s := unhex(t, "af46e36bf0527c9d3b16154b82465edd62144c0ac1fc5a18506a2244ba449aff")
	cases := []struct{ name, u, q string }{
		{"u0", "0000000000000000000000000000000000000000000000000000000000000000", ""},
		{"u1", "0100000000000000000000000000000000000000000000000000000000000000", ""},
		{"u2", "ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f", ""},
		{"u3", "e0eb7a7c3b41b8ae1656e3faf19fc46ada098deb9c32b1fd866205165f49b800", ""},
		{"u4", "5f9c95bca3508c24b1d0b1559c83ef5b04445cc4581c8e86d8224eddd09f1157", ""},
		{"u5", "edffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f", ""},
		{"u6", "daffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", "d8e2c776bbacd510d09fd9278b7edcd25fc5ae9adfba3b6e040e8d3b71b21806"},
		{"u7", "eeffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f", ""},
		{"u8", "dbffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", "c85c655ebe8be44ba9c0ffde69f2fe10194458d137f09bbff725ce58803cdb38"},
		{"u9", "d9ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", "db64dafa9b8fdd136914e61461935fe92aa372cb056314e1231bc4ec12417456"},
		{"ua", "cdeb7a7c3b41b8ae1656e3faf19fc46ada098deb9c32b1fd866205165f49b880", "e062dcd5376d58297be2618c7498f55baa07d7e03184e8aada20bca28888bf7a"},
		{"ub", "4c9c95bca3508c24b1d0b1559c83ef5b04445cc4581c8e86d8224eddd09f11d7", "993c6ad11c4c29da9a56f7691fd0ff8d732e49de6250b6c2e80003ff4629a175"},
	}
	for _, c := range cases {
		got, err := scalarMultVfy(s, unhex(t, c.u))
		if c.q == "" {
			if err != ErrAbort {
				t.Errorf("%s: want abort, got %x err=%v", c.name, got, err)
			}
			continue
		}
		if err != nil || hex.EncodeToString(got) != c.q {
			t.Errorf("%s = %x err=%v, want %s", c.name, got, err, c.q)
		}
	}
}

// A low-order share inside a protocol run must stop it, from either side.
func TestWeakShareAbortsTheExchange(t *testing.T) {
	sid := make([]byte, 16)
	a, _ := Start([]byte("12345678"), nil, sid, nil)
	b, _ := Start([]byte("12345678"), nil, sid, nil)
	zero := make([]byte, 32)
	one := append([]byte{1}, make([]byte, 31)...)
	for _, weak := range [][]byte{zero, one, make([]byte, 31), nil} {
		if _, err := a.FinishInitiator(weak, nil); err != ErrAbort {
			t.Errorf("initiator accepted a weak share %x: %v", weak, err)
		}
		if _, err := b.FinishResponder(weak, nil); err != ErrAbort {
			t.Errorf("responder accepted a weak share %x: %v", weak, err)
		}
	}
}

func run(t *testing.T, prsA, prsB string, ciA, ciB string, adaSeen, adbSeen string) (iskA, iskB []byte) {
	t.Helper()
	sid := []byte("0123456789abcdef")
	a, err := Start([]byte(prsA), []byte(ciA), sid, []byte("ada"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Start([]byte(prsB), []byte(ciB), sid, []byte("adb"))
	if err != nil {
		t.Fatal(err)
	}
	if iskA, err = a.FinishInitiator(b.Message(), []byte(adbSeen)); err != nil {
		t.Fatal(err)
	}
	if iskB, err = b.FinishResponder(a.Message(), []byte(adaSeen)); err != nil {
		t.Fatal(err)
	}
	return
}

func TestAgreementOnlyWhenEverythingMatches(t *testing.T) {
	a, b := run(t, "48217733", "48217733", "ci", "ci", "ada", "adb")
	if !bytes.Equal(a, b) || len(a) != 64 {
		t.Fatal("matching inputs gave different keys")
	}
	for name, got := range map[string][2][]byte{
		"wrong PIN":             pair(run(t, "48217733", "48217734", "ci", "ci", "ada", "adb")),
		"different channel":     pair(run(t, "48217733", "48217733", "ci", "other", "ada", "adb")),
		"tampered initiator AD": pair(run(t, "48217733", "48217733", "ci", "ci", "evil", "adb")),
		"tampered responder AD": pair(run(t, "48217733", "48217733", "ci", "ci", "ada", "evil")),
	} {
		if bytes.Equal(got[0], got[1]) {
			t.Errorf("%s: the two sides still agree", name)
		}
	}
	a2, _ := run(t, "48217733", "48217733", "ci", "ci", "ada", "adb")
	if bytes.Equal(a, a2) {
		t.Error("two runs gave the same key: shares are not fresh")
	}
}

func pair(a, b []byte) [2][]byte { return [2][]byte{a, b} }
