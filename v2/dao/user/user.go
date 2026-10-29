package user

import (
	"GopherAI/common/mysql"
	"GopherAI/model"
	"errors"

	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

func FindByLogin(identifier string) (*model.User, error) {
	return mysql.GetUserByLogin(identifier)
}

func EmailExists(email string) (bool, error) {
	return mysql.UserEmailExists(email)
}

func Register(username, email, passwordHash string) (*model.User, error) {
	return mysql.InsertUser(&model.User{
		Email:    email,
		Name:     username,
		Username: username,
		Password: passwordHash,
	})
}

func UpgradePassword(userID int64, expectedHash, replacementHash string) (bool, error) {
	return mysql.UpdateUserPasswordIfMatch(userID, expectedHash, replacementHash)
}

func IsNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound)
}

func IsDuplicate(err error) bool {
	var mysqlErr *mysqldriver.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1062
}
