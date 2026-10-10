package terminal

import "testing"

func TestValidateEnv_RejectsLoaderVars(t *testing.T) {
	for _, k := range []string{"LD_PRELOAD", "LD_LIBRARY_PATH", "DYLD_INSERT_LIBRARIES", "BASH_ENV", "PATH", "YOAKELAND_MASTER_KEY"} {
		if _, err := validateEnv(map[string]string{k: "x"}); err == nil {
			t.Errorf("%s accepted", k)
		}
	}
	if out, err := validateEnv(map[string]string{"FOO": "bar"}); err != nil || len(out) != 1 {
		t.Fatalf("plain var rejected: %v %v", out, err)
	}
}
