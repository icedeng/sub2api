package wayviaegress

import (
	"context"
	"testing"
	"time"
)

func TestBeforeDeleteRetiresAccountWithUncertainCredentials(t *testing.T) {
	m := recoveryFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := m.db.ExecContext(ctx, `
		UPDATE accounts SET schedulable=false WHERE id=1;
		UPDATE wayvia_account_egress_policies
		SET state='credential_uncertain',last_error='EGRESS_CREDENTIAL_UNCERTAIN'
		WHERE root_account_id=1`); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	before := m.policies[1]
	if _, err := m.Resolve(1, "refresh"); err == nil {
		t.Fatal("uncertain credentials must still block refresh")
	}
	if _, err := m.db.ExecContext(ctx, `UPDATE accounts SET deleted_at=now() WHERE id=1`); err == nil {
		t.Fatal("account deletion must still require retirement")
	}

	if err := m.BeforeDelete(ctx, 1); err != nil {
		t.Fatal(err)
	}
	m.operationWG.Wait()
	after := m.policies[1]
	if after.State != "retired" || after.Owner != before.Owner || after.BindingID != before.BindingID {
		t.Fatal("retirement must preserve the original owner and binding", after)
	}
	if _, err := m.Resolve(1, "business"); err == nil {
		t.Fatal("retired account must remain closed to business traffic")
	}
	if _, err := m.db.ExecContext(ctx, `UPDATE accounts SET deleted_at=now() WHERE id=1`); err != nil {
		t.Fatal("retired account should be deletable", err)
	}
}
