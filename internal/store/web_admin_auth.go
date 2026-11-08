package store

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

// WebAdminLoginState 是一次登录请求面临的限制状态。Delay 是最近 15 分钟
// 全部失败造成的等待；Locked 与 LockRemaining 描述来源 IP 的锁定。
type WebAdminLoginState struct {
	Locked        bool
	LockRemaining time.Duration
	Delay         time.Duration
}

// webAdminLoginState 是登录失败记录在 vault 里的持久形态。时间统一保存
// Unix 秒；读取时按调用方传入的当前时间裁剪过期事件。
type webAdminLoginState struct {
	IPFailures     map[string][]int64 `json:"ip_failures"`
	GlobalFailures []int64            `json:"global_failures"`
	IPLocks        map[string]int64   `json:"ip_locks"`
}

const (
	webAdminFailureWindow = 15 * time.Minute
	webAdminLockDuration  = 15 * time.Minute
	webAdminMaxLoginDelay = 10 * time.Second
)

// WebAdminPasswordIsSet 报告 owner 是否已经设置 Web 管理员口令。
func (s *Store) WebAdminPasswordIsSet() (bool, error) {
	hash, err := s.WebAdminPasswordHash()
	if err != nil {
		return false, err
	}
	return hash != "", nil
}

// WebAdminPasswordHash 返回自描述的 argon2id 口令哈希。它只供 owner 侧诊
// 断与备份使用；登录校验走 VerifyWebAdminPassword。
func (s *Store) WebAdminPasswordHash() (string, error) {
	var hash string
	err := s.db.QueryRow(`SELECT password_hash FROM web_admin_auth WHERE singleton=1`).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return hash, err
}

// VerifyWebAdminPassword 用常量时间比较校验管理员口令。
func (s *Store) VerifyWebAdminPassword(password []byte) (bool, error) {
	encoded, err := s.WebAdminPasswordHash()
	if err != nil {
		return false, err
	}
	if encoded == "" {
		return false, nil
	}
	return verifyWebAdminPasswordHash(password, encoded)
}

// SetWebAdminPassword 生成新的随机盐并保存 argon2id 哈希，同时把会话代加
// 一，让旧 Web 会话立即失效。返回新的会话代。
func (s *Store) SetWebAdminPassword(password []byte) (int64, error) {
	encoded, err := encodeWebAdminPassword(password)
	if err != nil {
		return 0, err
	}
	return s.updateWebAdminAuth(func(current webAdminAuthRow) (webAdminAuthRow, error) {
		current.PasswordHash = encoded
		current.SessionGeneration++
		return current, nil
	})
}

// WebAdminSessionGeneration 返回当前 Web 会话代。
func (s *Store) WebAdminSessionGeneration() (int64, error) {
	row, err := readWebAdminAuthRow(s.db)
	if err != nil {
		return 0, err
	}
	return row.SessionGeneration, nil
}

// LogoutAllWebAdminSessions 把会话代加一，作废所有运行中 serve 进程里的
// Web 会话，并返回新的会话代。
func (s *Store) LogoutAllWebAdminSessions() (int64, error) {
	return s.updateWebAdminAuth(func(current webAdminAuthRow) (webAdminAuthRow, error) {
		current.SessionGeneration++
		return current, nil
	})
}

// WebAdminLoginState 返回指定来源 IP 当前面临的锁定与全局延迟。
func (s *Store) WebAdminLoginState(ip string, at time.Time) (WebAdminLoginState, error) {
	row, err := readWebAdminAuthRow(s.db)
	if err != nil {
		return WebAdminLoginState{}, err
	}
	state := pruneWebAdminLoginState(row.LoginState, at)
	return webAdminLoginStateView(state, ip, at), nil
}

// RecordWebAdminLoginFailure 记录一次失败，按 15 分钟窗口维护按 IP 锁定与
// 全局延迟，并把结果返回给请求处理器。
func (s *Store) RecordWebAdminLoginFailure(ip string, at time.Time) (WebAdminLoginState, error) {
	var view WebAdminLoginState
	_, err := s.updateWebAdminAuth(func(current webAdminAuthRow) (webAdminAuthRow, error) {
		state := pruneWebAdminLoginState(current.LoginState, at)
		state.IPFailures[ip] = append(state.IPFailures[ip], at.Unix())
		state.GlobalFailures = append(state.GlobalFailures, at.Unix())
		if len(state.IPFailures[ip]) >= 5 {
			state.IPLocks[ip] = at.Add(webAdminLockDuration).Unix()
		}
		current.LoginState = state
		view = webAdminLoginStateView(state, ip, at)
		return current, nil
	})
	return view, err
}

// ClearWebAdminLoginFailures 删除指定来源 IP 的失败与锁定记录，供权威端
// 主机上的管理员命令解除锁定。
func (s *Store) ClearWebAdminLoginFailures(ip string) error {
	_, err := s.updateWebAdminAuth(func(current webAdminAuthRow) (webAdminAuthRow, error) {
		state := pruneWebAdminLoginState(current.LoginState, time.Now())
		delete(state.IPFailures, ip)
		delete(state.IPLocks, ip)
		current.LoginState = state
		return current, nil
	})
	return err
}

type webAdminAuthRow struct {
	PasswordHash      string
	SessionGeneration int64
	LoginState        webAdminLoginState
}

type webAdminAuthDB dbtx

func (s *Store) updateWebAdminAuth(change func(webAdminAuthRow) (webAdminAuthRow, error)) (int64, error) {
	var generation int64
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() {
		_ = tx.Rollback()
	}()
	if _, err := tx.Exec(`INSERT OR IGNORE INTO web_admin_auth(singleton,password_hash,session_generation,login_state) VALUES(1,'',0,'{}')`); err != nil {
		return 0, err
	}
	err = func() error {
		row, err := readWebAdminAuthRow(tx)
		if err != nil {
			return err
		}
		changed, err := change(row)
		if err != nil {
			return err
		}
		encoded, err := marshalWebAdminLoginState(changed.LoginState)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
			UPDATE web_admin_auth
			SET password_hash=?, session_generation=?, login_state=?
			WHERE singleton=1`,
			changed.PasswordHash, changed.SessionGeneration, encoded,
		); err != nil {
			return err
		}
		generation = changed.SessionGeneration
		return nil
	}()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return generation, err
}

func readWebAdminAuthRow(db webAdminAuthDB) (webAdminAuthRow, error) {
	var row webAdminAuthRow
	var encoded string
	err := db.QueryRow(`SELECT password_hash,session_generation,login_state FROM web_admin_auth WHERE singleton=1`).
		Scan(&row.PasswordHash, &row.SessionGeneration, &encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return webAdminAuthRow{LoginState: emptyWebAdminLoginState()}, nil
	}
	if err != nil {
		return row, err
	}
	if err := unmarshalWebAdminLoginState(encoded, &row.LoginState); err != nil {
		return row, err
	}
	return row, nil
}

func emptyWebAdminLoginState() webAdminLoginState {
	return webAdminLoginState{
		IPFailures:     map[string][]int64{},
		GlobalFailures: []int64{},
		IPLocks:        map[string]int64{},
	}
}

func unmarshalWebAdminLoginState(encoded string, out *webAdminLoginState) error {
	*out = emptyWebAdminLoginState()
	if strings.TrimSpace(encoded) == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(encoded), out); err != nil {
		return fmt.Errorf("解析 Web 管理员登录状态失败：%w", err)
	}
	if out.IPFailures == nil {
		out.IPFailures = map[string][]int64{}
	}
	if out.GlobalFailures == nil {
		out.GlobalFailures = []int64{}
	}
	if out.IPLocks == nil {
		out.IPLocks = map[string]int64{}
	}
	return nil
}

func marshalWebAdminLoginState(state webAdminLoginState) (string, error) {
	if state.IPFailures == nil && state.GlobalFailures == nil && state.IPLocks == nil {
		return "{}", nil
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func pruneWebAdminLoginState(state webAdminLoginState, now time.Time) webAdminLoginState {
	cutoff := now.Add(-webAdminFailureWindow).Unix()
	for ip, times := range state.IPFailures {
		kept := make([]int64, 0, len(times))
		for _, ts := range times {
			if ts > cutoff {
				kept = append(kept, ts)
			}
		}
		if len(kept) == 0 {
			delete(state.IPFailures, ip)
		} else {
			state.IPFailures[ip] = kept
		}
	}
	keptGlobal := make([]int64, 0, len(state.GlobalFailures))
	for _, ts := range state.GlobalFailures {
		if ts > cutoff {
			keptGlobal = append(keptGlobal, ts)
		}
	}
	state.GlobalFailures = keptGlobal
	for ip, until := range state.IPLocks {
		if until <= now.Unix() {
			delete(state.IPLocks, ip)
		}
	}
	return state
}

func webAdminLoginStateView(state webAdminLoginState, ip string, now time.Time) WebAdminLoginState {
	view := WebAdminLoginState{
		Delay: time.Duration(len(state.GlobalFailures)) * time.Second,
	}
	if view.Delay > webAdminMaxLoginDelay {
		view.Delay = webAdminMaxLoginDelay
	}
	if until, ok := state.IPLocks[ip]; ok {
		if until > now.Unix() {
			view.Locked = true
			view.LockRemaining = time.Duration(until-now.Unix()) * time.Second
		}
	}
	return view
}

func encodeWebAdminPassword(password []byte) (string, error) {
	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return "", err
	}
	const (
		timeCost = 1
		memory   = 64 * 1024
		threads  = 4
		keyLen   = 32
	)
	digest := argon2.IDKey(password, salt, timeCost, memory, threads, keyLen)
	encoding := base64.RawStdEncoding
	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, memory, timeCost, threads,
		encoding.EncodeToString(salt), encoding.EncodeToString(digest),
	), nil
}

func verifyWebAdminPasswordHash(password []byte, encoded string) (bool, error) {
	fields := strings.Split(encoded, "$")
	if len(fields) != 6 || fields[0] != "" || fields[1] != "argon2id" {
		return false, fmt.Errorf("管理员口令哈希格式不支持：%q", encoded)
	}
	if fields[2] != "v="+strconv.Itoa(argon2.Version) {
		return false, fmt.Errorf("管理员口令哈希版本不支持：%q", fields[2])
	}
	parameterText := strings.Split(fields[3], ",")
	if len(parameterText) != 3 || !strings.HasPrefix(parameterText[0], "m=") ||
		!strings.HasPrefix(parameterText[1], "t=") || !strings.HasPrefix(parameterText[2], "p=") {
		return false, fmt.Errorf("管理员口令哈希参数格式不支持：%q", fields[3])
	}
	values := make([]int, 3)
	for i, text := range parameterText {
		value, err := strconv.Atoi(text[2:])
		if err != nil || value <= 0 {
			return false, fmt.Errorf("管理员口令哈希参数非法：%q", text)
		}
		values[i] = value
	}
	encoding := base64.RawStdEncoding
	salt, err := encoding.DecodeString(fields[4])
	if err != nil {
		return false, fmt.Errorf("管理员口令哈希盐非法：%w", err)
	}
	want, err := encoding.DecodeString(fields[5])
	if err != nil {
		return false, fmt.Errorf("管理员口令哈希值非法：%w", err)
	}
	got := argon2.IDKey(password, salt, uint32(values[1]), uint32(values[0]), uint8(values[2]), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
