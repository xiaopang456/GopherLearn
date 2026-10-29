package redis

import (
	"context"
	"os"
	"testing"
	"time"

	redisCli "github.com/redis/go-redis/v9"
)

func TestCaptchaReservationAndAtomicConsumption(t *testing.T) {
	addr := os.Getenv("GOPHERAI_REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("set GOPHERAI_REDIS_TEST_ADDR to run Redis captcha integration test")
	}
	previous := Rdb
	Rdb = redisCli.NewClient(&redisCli.Options{Addr: addr, Password: os.Getenv("GOPHERAI_REDIS_TEST_PASSWORD"), Protocol: 2})
	t.Cleanup(func() {
		_ = Rdb.Close()
		Rdb = previous
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := Rdb.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	email := "stabilize006-captcha@example.invalid"
	t.Cleanup(func() {
		_ = Rdb.Del(context.Background(), GenerateCaptcha(email), GenerateCaptchaCooldown(email)).Err()
	})
	if err := Rdb.Del(ctx, GenerateCaptcha(email), GenerateCaptchaCooldown(email)).Err(); err != nil {
		t.Fatal(err)
	}
	reserved, err := ReserveCaptchaSend(email)
	if err != nil || !reserved {
		t.Fatalf("first reservation reserved=%v err=%v", reserved, err)
	}
	reserved, err = ReserveCaptchaSend(email)
	if err != nil || reserved {
		t.Fatalf("second reservation reserved=%v err=%v", reserved, err)
	}
	if err := SetCaptchaForEmail(email, "123456"); err != nil {
		t.Fatal(err)
	}
	if valid, err := VerifyCaptchaForEmail(email, "654321"); err != nil || valid {
		t.Fatalf("wrong captcha valid=%v err=%v", valid, err)
	}
	if valid, err := VerifyCaptchaForEmail(email, "123456"); err != nil || !valid {
		t.Fatalf("correct captcha valid=%v err=%v", valid, err)
	}
	if consumed, err := ConsumeCaptchaForEmail(email, "123456"); err != nil || !consumed {
		t.Fatalf("consume valid captcha consumed=%v err=%v", consumed, err)
	}
	if consumed, err := ConsumeCaptchaForEmail(email, "123456"); err != nil || consumed {
		t.Fatalf("second consume consumed=%v err=%v", consumed, err)
	}
	if err := ReleaseCaptchaSend(email); err != nil {
		t.Fatal(err)
	}
	if reserved, err := ReserveCaptchaSend(email); err != nil || !reserved {
		t.Fatalf("reservation after release reserved=%v err=%v", reserved, err)
	}
}
