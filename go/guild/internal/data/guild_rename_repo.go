package data

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"guild/internal/constants"
)

func randomGuildHoldID() (uint64, error) {
	var buf [8]byte
	for i := 0; i < 4; i++ {
		if _, err := rand.Read(buf[:]); err != nil {
			return 0, fmt.Errorf("mint guild name hold id: %w", err)
		}
		// 保留非 0 正数范围(63 位),兼容无符号/有符号工具。
		if id := binary.BigEndian.Uint64(buf[:]) & ((1 << 63) - 1); id != 0 {
			return id, nil
		}
	}
	return 0, errors.New("failed to mint non-zero guild name hold id")
}

// claimGuildNameHold 校验新帮名是否在保护表中且未过期。
// 调用前提(docs/design/rename-and-content-safety.md §5):本事务已经完成对 guild 表的 INSERT 或 UPDATE
// (持有 uk_guild(name_norm) 唯一索引锁),因此不会与正在把该名放进保护表的并发改名事务交错。
// 若该名属于他帮且保护未到期则返回 ErrGuildNameTaken;若已过期或属于本帮则顺手删掉保护行。
func claimGuildNameHold(ctx context.Context, tx *sql.Tx, guildID uint64, nameNorm string, nowMs uint64) error {
	var holdID, holderGuildID, heldUntilMs uint64
	err := tx.QueryRowContext(ctx,
		`SELECT hold_id, guild_id, held_until_ms FROM guild_name_hold WHERE name_norm = ? FOR UPDATE`,
		nameNorm).Scan(&holdID, &holderGuildID, &heldUntilMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock guild name hold for %q: %w", nameNorm, err)
	}
	if holderGuildID != guildID && heldUntilMs > nowMs {
		return ErrGuildNameTaken
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM guild_name_hold WHERE hold_id = ?`, holdID); err != nil {
		return fmt.Errorf("delete expired/self guild name hold %d: %w", holdID, err)
	}
	return nil
}

// holdGuildName 将旧帮名写入保护表(到期时刻 heldUntilMs)。若同 name_norm 已有旧记录则原地刷新。
func holdGuildName(ctx context.Context, tx *sql.Tx, guildID uint64, oldNorm string, heldUntilMs uint64) error {
	var holdID uint64
	err := tx.QueryRowContext(ctx,
		`SELECT hold_id FROM guild_name_hold WHERE name_norm = ? FOR UPDATE`,
		oldNorm).Scan(&holdID)
	if err == nil {
		if _, err := tx.ExecContext(ctx,
			`UPDATE guild_name_hold SET guild_id = ?, held_until_ms = ? WHERE hold_id = ?`,
			guildID, heldUntilMs, holdID); err != nil {
			return fmt.Errorf("update guild name hold %d: %w", holdID, err)
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("lock old guild name hold for %q: %w", oldNorm, err)
	}
	newID, err := randomGuildHoldID()
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO guild_name_hold (hold_id, name_norm, guild_id, held_until_ms) VALUES (?, ?, ?, ?)`,
		newID, oldNorm, guildID, heldUntilMs); err != nil {
		return fmt.Errorf("insert guild name hold for guild %d: %w", guildID, err)
	}
	return nil
}

// RenameGuild 在单个本地事务中完成帮会改名(docs/design/rename-and-content-safety.md §5)。
//
// 严格锁序:
//  1. 锁 guild 行:核帮主、同名、冷却、资金,再锁帮主成员行复核权威 role;
//  2. 先 UPDATE guild.name / name_norm(在 uk_guild 上判重并序列化同名抢注);
//  3. 再查 guild_name_hold(新名被他帮未过期保护则回滚返回 ErrGuildNameTaken,过期或本帮则删除);
//  4. 若 hold > 0,将旧名写入 guild_name_hold;
//  5. 扣资金并写 renamed_ms,读取事务内权威快照提交并失效缓存。
//
// 返回值:成功时返回最新快照与下次可改名时刻(Unix 毫秒);冷却拒绝时返回 (nil, nextAllowedMs, ErrGuildRenameCooldown)。
func (r *GuildRepo) RenameGuild(ctx context.Context, guildID, leaderID uint64, newName string,
	cooldown, hold time.Duration, costFunds uint64, nowMs uint64) (*GuildData, uint64, error) {
	newNorm, ok := GuildNameNorm(newName)
	if !ok {
		return nil, 0, fmt.Errorf("rename guild %d: name fails normalization", guildID)
	}

	var (
		out           *GuildData
		nextAllowedMs uint64
	)
	cooldownMs := uint64(0)
	if cooldown > 0 {
		cooldownMs = uint64(cooldown.Milliseconds())
	}
	holdMs := uint64(0)
	if hold > 0 {
		holdMs = uint64(hold.Milliseconds())
	}

	err := r.inTx(withRoleTargets(ctx, leaderID), opRename, func(ctx context.Context, tx *sql.Tx) error {
		var (
			oldNorm   string
			curLeader uint64
			funds     uint64
			renamedMs uint64
		)
		err := tx.QueryRowContext(ctx,
			`SELECT name_norm, leader_id, funds, renamed_ms FROM guild WHERE guild_id = ? FOR UPDATE`,
			guildID).Scan(&oldNorm, &curLeader, &funds, &renamedMs)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrGuildGone
		}
		if err != nil {
			return fmt.Errorf("lock guild %d for rename: %w", guildID, err)
		}

		// 核对成员表权威身份(guild → guild_member)。
		var role uint32
		err = tx.QueryRowContext(ctx, sqlLockMemberRole, guildID, leaderID).Scan(&role)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotGuildMember
		}
		if err != nil {
			return fmt.Errorf("lock rename operator %d in guild %d: %w", leaderID, guildID, err)
		}
		if curLeader != leaderID || constants.Rank(role) != constants.RankLeader {
			return ErrRankTooLow
		}
		if newNorm == oldNorm {
			return ErrGuildRenameSameName
		}
		if renamedMs > 0 && cooldownMs > 0 && nowMs < renamedMs+cooldownMs {
			nextAllowedMs = renamedMs + cooldownMs
			return ErrGuildRenameCooldown
		}
		if funds < costFunds {
			return ErrFundsInsufficient
		}

		// 第 2 步必须在第 3 步之前(§5):先更新 guild 拿到 uk_guild 唯一索引锁。
		if _, err := tx.ExecContext(ctx,
			`UPDATE guild SET name = ?, name_norm = ? WHERE guild_id = ?`,
			newName, newNorm, guildID); err != nil {
			if isDuplicateKeyOn(err, guildNameUniqueKey) {
				return ErrGuildNameTaken
			}
			return fmt.Errorf("update guild %d name: %w", guildID, err)
		}

		// 第 3 步:检查新名是否在他帮的保护期内。
		if err := claimGuildNameHold(ctx, tx, guildID, newNorm, nowMs); err != nil {
			return err
		}

		// 第 4 步:写入旧名保护期。
		if holdMs > 0 && oldNorm != "" {
			if err := holdGuildName(ctx, tx, guildID, oldNorm, nowMs+holdMs); err != nil {
				return err
			}
		}

		// 第 5 步:扣帮会资金并记录改名时刻。
		if err := execExactlyOneRow(ctx, tx,
			fmt.Sprintf("deduct funds and stamp renamed_ms for guild %d", guildID),
			`UPDATE guild SET funds = funds - ?, renamed_ms = ? WHERE guild_id = ? AND funds >= ?`,
			costFunds, nowMs, guildID, costFunds); err != nil {
			return err
		}

		snapshot, err := txSnapshot(ctx, tx, guildID)
		if err != nil {
			return err
		}
		out = snapshot
		if cooldownMs > 0 {
			nextAllowedMs = nowMs + cooldownMs
		}
		return nil
	})
	if err != nil {
		return nil, nextAllowedMs, err
	}
	r.invalidateAfterCommit(ctx, opRename, guildID)
	return out, nextAllowedMs, nil
}
