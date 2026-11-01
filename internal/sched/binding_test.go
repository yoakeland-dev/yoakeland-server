package sched

import "testing"

func TestBindingSurvivesARestartAndIsForgotten(t *testing.T) {
	s := New(t.TempDir())
	s.RememberBinding("s1", Binding{Provider: "claude", Model: "haiku"})
	s.RememberBinding("s1", Binding{Provider: "claude", Model: "haiku"}) // unchanged: not saved again
	again := New(s.Dir())
	if b, ok := again.BindingFor("s1"); !ok || b.Provider != "claude" || b.Model != "haiku" {
		t.Fatalf("binding = %+v %v", b, ok)
	}
	again.Forget("s1")
	if _, ok := New(s.Dir()).BindingFor("s1"); ok {
		t.Fatal("binding outlived its chat")
	}
}
