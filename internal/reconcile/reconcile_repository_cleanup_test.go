package reconcile

import (
	"context"
	"reflect"
	"testing"
)

type repositoryCleanupNotificationHooks struct{ notifications [][]string }

func (*repositoryCleanupNotificationHooks) PurgeCheckoutLayers(context.Context, string, string) error {
	return nil
}
func (*repositoryCleanupNotificationHooks) ReleaseGraph(context.Context, string) error { return nil }
func (h *repositoryCleanupNotificationHooks) RepositoryCleanupAttemptFinished(graphs []string) {
	h.notifications = append(h.notifications, append([]string(nil), graphs...))
}

func TestRepositoryCleanupNotifiesOnlyAfterOutermostSaga(t *testing.T) {
	hooks := &repositoryCleanupNotificationHooks{}
	r := &Reconciler{hooks: hooks}
	ctx, finishOuter := r.repositoryCleanupAttempt(context.Background(), "primary")
	nested, finishInner := r.repositoryCleanupAttempt(ctx, "dependent")
	_, finishRepeated := r.repositoryCleanupAttempt(nested, "primary")
	finishRepeated()
	finishInner()
	if len(hooks.notifications) != 0 {
		t.Fatal("nested completion can race outer phase persistence")
	}
	finishOuter() // The runSaga defer runs after its final persistence/return.
	want := [][]string{{"dependent", "primary"}}
	if !reflect.DeepEqual(hooks.notifications, want) {
		t.Fatalf("notifications=%v want=%v", hooks.notifications, want)
	}
}

func TestRepositoryCleanupAttemptDoesNotJoinAnotherReconciler(t *testing.T) {
	first, second := &repositoryCleanupNotificationHooks{}, &repositoryCleanupNotificationHooks{}
	r1, r2 := &Reconciler{hooks: first}, &Reconciler{hooks: second}
	ctx, finish1 := r1.repositoryCleanupAttempt(context.Background(), "first")
	_, finish2 := r2.repositoryCleanupAttempt(ctx, "second")
	finish2()
	if len(first.notifications) != 0 || !reflect.DeepEqual(second.notifications, [][]string{{"second"}}) {
		t.Fatal("different reconciler inherited outer completion ownership")
	}
	finish1()
	if !reflect.DeepEqual(first.notifications, [][]string{{"first"}}) {
		t.Fatal(first.notifications)
	}
}
