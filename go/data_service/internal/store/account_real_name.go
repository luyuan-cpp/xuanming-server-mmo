package store

import (
	"context"
	"database/sql"
	"errors"

	"shared/accountdirectory"
	"shared/antiaddiction"
)

// 账号行上的实名列 real_name_pi(docs/design/real-name-anti-addiction.md §5.1):国家防沉迷系统给的 pi,只写一次,
// 不存姓名和身份证号。年龄由调用方从 pi 的出生日期段算。

// SetRealName 把 pi 写到账号行上,只在当前为空时写;返回写之后库里的值。已有相同的 pi 算成功(重放),
// 已有不同的 pi 不改(换实名信息走客服),由调用方比较。账号行不存在回 ErrMissing。
func (s *AccountDirectoryStore) SetRealName(ctx context.Context, account, pi string) (string, error) {
	if !accountdirectory.ValidAccount(account) || !antiaddiction.ValidPI(pi) {
		return "", accountdirectory.ErrInvalid
	}
	var stored []byte
	err := shardTx(ctx, s.shards.forAccount(account), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE account_directory SET real_name_pi=? WHERE account=? AND real_name_pi=''",
			[]byte(pi), []byte(account)); err != nil {
			return err
		}
		err := tx.QueryRowContext(ctx, "SELECT real_name_pi FROM account_directory WHERE account=?", []byte(account)).Scan(&stored)
		if errors.Is(err, sql.ErrNoRows) {
			return accountdirectory.ErrMissing
		}
		return err
	})
	if err != nil {
		return "", err
	}
	return string(stored), nil
}

// ResetRealName 清掉账号行上的 pi(客服重置,§5.2 末),返回清之前有没有。幂等:没有 pi 的账号什么都不改。
// 账号行不存在回 ErrMissing。
func (s *AccountDirectoryStore) ResetRealName(ctx context.Context, account string) (bool, error) {
	if !accountdirectory.ValidAccount(account) {
		return false, accountdirectory.ErrInvalid
	}
	var had bool
	err := shardTx(ctx, s.shards.forAccount(account), func(tx *sql.Tx) error {
		var pi []byte
		err := tx.QueryRowContext(ctx, "SELECT real_name_pi FROM account_directory WHERE account=? FOR UPDATE", []byte(account)).Scan(&pi)
		if errors.Is(err, sql.ErrNoRows) {
			return accountdirectory.ErrMissing
		}
		if err != nil {
			return err
		}
		if had = len(pi) > 0; !had {
			return nil
		}
		_, err = tx.ExecContext(ctx, "UPDATE account_directory SET real_name_pi='' WHERE account=?", []byte(account))
		return err
	})
	if err != nil {
		return false, err
	}
	return had, nil
}

// RealNameOfPlayer 回玩家所属账号的 pi(充值限额闸用,§5.6)。角色不在目录里、已删,或账号行已注销删除,回 found=false。
// 角色行与账号行在不同的片上,两次点读不在一个事务里:pi 只写一次,先读角色、后读账号不会读到矛盾的值。
func (s *AccountDirectoryStore) RealNameOfPlayer(ctx context.Context, player uint64) (string, bool, error) {
	owned, err := s.Lookup(ctx, player)
	if errors.Is(err, accountdirectory.ErrMissing) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	var pi []byte
	err = s.shards.forAccount(owned.Account).QueryRowContext(ctx,
		"SELECT real_name_pi FROM account_directory WHERE account=?", []byte(owned.Account)).Scan(&pi)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(pi), true, nil
}
