package redis

import (
	"GopherAI/config"
	"context"
	"strconv"
	"time"

	redisCli "github.com/redis/go-redis/v9"
)

var Rdb *redisCli.Client

var ctx = context.Background()

const (
	CaptchaTTL          = 2 * time.Minute
	CaptchaSendCooldown = 60 * time.Second
)

var consumeCaptchaScript = redisCli.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`)

func Init() {
	conf := config.GetConfig()
	host := conf.RedisConfig.RedisHost
	port := conf.RedisConfig.RedisPort
	password := conf.RedisConfig.RedisPassword
	db := conf.RedisDb
	addr := host + ":" + strconv.Itoa(port)

	Rdb = redisCli.NewClient(&redisCli.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
		Protocol: 2, // 使用 Protocol 2 避免 maint_notifications 警告
	})

}

func SetCaptchaForEmail(email, captcha string) error {
	key := GenerateCaptcha(email)
	return Rdb.Set(ctx, key, captcha, CaptchaTTL).Err()
}

func VerifyCaptchaForEmail(email, userInput string) (bool, error) {
	key := GenerateCaptcha(email)
	storedCaptcha, err := Rdb.Get(ctx, key).Result()
	if err != nil {
		if err == redisCli.Nil {
			return false, nil
		}
		return false, err
	}
	return storedCaptcha == userInput, nil
}

func ConsumeCaptchaForEmail(email, userInput string) (bool, error) {
	result, err := consumeCaptchaScript.Run(ctx, Rdb, []string{GenerateCaptcha(email)}, userInput).Int64()
	return result == 1, err
}

func DeleteCaptchaForEmail(email string) error {
	return Rdb.Del(ctx, GenerateCaptcha(email)).Err()
}

func ReserveCaptchaSend(email string) (bool, error) {
	return Rdb.SetNX(ctx, GenerateCaptchaCooldown(email), "1", CaptchaSendCooldown).Result()
}

func ReleaseCaptchaSend(email string) error {
	return Rdb.Del(ctx, GenerateCaptchaCooldown(email)).Err()
}
