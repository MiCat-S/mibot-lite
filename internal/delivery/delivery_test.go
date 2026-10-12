package delivery

import (
	"context"
	"errors"
	"testing"
)

func TestUnknownKeepsMessageAndChain(t *testing.T) {
	if Unknown(nil) != nil || Uncertain(nil) || Uncertain(errors.New("x")) {
		t.Fatal("nil 或普通错误被当成不确定")
	}
	err := Unknown(context.Canceled)
	if !Uncertain(err) || !errors.Is(err, context.Canceled) || err.Error() != context.Canceled.Error() {
		t.Fatalf("包装后：%v", err)
	}
	if wrapped := errors.Join(errors.New("外层"), err); !Uncertain(wrapped) {
		t.Fatal("再包一层后认不出")
	}
}
