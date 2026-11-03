package cloudauth

import (
	"strings"
	"testing"

	"github.com/royosherove/ajx/internal/plugin"
	"github.com/royosherove/ajx/internal/util"
)

func TestMatchesAndSupplied(t *testing.T) {
	if !Matches("AWS_CONTAINER_X", []string{"AWS_CONTAINER_*"}) || Matches("AWS_CONTAINE", []string{"AWS_CONTAINER_*"}) ||
		!Matches("A", []string{"B", "A"}) || Matches("AB", []string{"A"}) {
		t.Fatal("matches")
	}
	env := map[string]string{"A": "x", "B": "  ", "C": "${UNSET_REF}", "D": "pre-${X}-post"}
	if !Supplied(env, "A") || Supplied(env, "B") || Supplied(env, "C") || Supplied(env, "D") || Supplied(env, "E") {
		t.Fatal("supplied")
	}
}

func synthetic(conf util.M) *CloudAuth {
	a := New(conf)
	a.PluginName = "synthetic-cloud"
	a.Self = a
	a.ProviderEnv = "USE_SYNTHETIC"
	a.CredentialRoutes = [][]string{{"KEY_ID", "KEY_SECRET"}, {"TOKEN"}}
	a.UnsupportedEnv = []string{"PROFILE", "FILE_*"}
	a.FixedEnv = map[string]string{"USE_SYNTHETIC": "1"}
	a.BaseUnset = []string{"OTHER_PROVIDER"}
	return a
}

func TestEnvironmentProblems(t *testing.T) {
	a := synthetic(util.M{"env": util.M{"TOKEN": "t"}})
	var _ plugin.Auth = a
	if p := a.EnvironmentProblems(a.Env(), nil); len(p) != 0 {
		t.Fatal(p)
	}
	if got := a.Unset(); strings.Join(got, ",") != "OTHER_PROVIDER" {
		t.Fatal(got)
	}
	p := strings.Join(synthetic(nil).EnvironmentProblems(map[string]string{"KEY_ID": "x", "FILE_PATH": "/x", "PROFILE": ""}, nil), "\n")
	for _, want := range []string{
		"set USE_SYNTHETIC=1 (or true) and do not unset it",
		"synthetic-cloud does not support FILE_PATH in explicit environments",
		"synthetic-cloud requires explicitly declared KEY_ID + KEY_SECRET or TOKEN in the worker env",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("%q missing from\n%s", want, p)
		}
	}
	if strings.Contains(p, "PROFILE,") || strings.Contains(p, "/x") {
		t.Fatal(p)
	}
	noRoute := synthetic(nil)
	noRoute.CredentialRoutes = nil
	noRoute.HomeCredentials = "synthetic logins"
	p = strings.Join(noRoute.EnvironmentProblems(map[string]string{"USE_SYNTHETIC": "true"}, nil), "\n")
	if !strings.Contains(p, "synthetic-cloud has no supported direct-env credential route in explicit environments: synthetic logins are not provisioned") {
		t.Fatal(p)
	}
	// the profile's unset applies before checking; required vars are named, never valued
	a = synthetic(util.M{"env": util.M{"TOKEN": "secret-value"}, "unset": []any{"TOKEN"}})
	a.RequiredEnv = []string{"REGION"}
	p = strings.Join(a.EnvironmentProblems(a.Env(), nil), "\n")
	if !strings.Contains(p, "missing env var REGION") || !strings.Contains(p, "TOKEN") || strings.Contains(p, "secret-value") {
		t.Fatal(p)
	}
}

func TestRegionAliasIsMirroredAndModeled(t *testing.T) {
	a := synthetic(util.M{"env": util.M{"AWS_REGION": "eu-west-1"}})
	if a.Env()["AWS_DEFAULT_REGION"] != "eu-west-1" || a.Env()["USE_SYNTHETIC"] != "1" {
		t.Fatal(a.Env())
	}
	if !util.Contains(a.ModelVars(nil), "AWS_DEFAULT_REGION") {
		t.Fatal(a.ModelVars(nil))
	}
	if util.Contains(synthetic(nil).ModelVars(nil), "AWS_DEFAULT_REGION") {
		t.Fatal("alias is only modeled when declared")
	}
	b := synthetic(util.M{"env": util.M{"AWS_REGION": " "}})
	if _, ok := b.Env()["AWS_DEFAULT_REGION"]; ok {
		t.Fatal("blank values are not mirrored")
	}
}
