package clientops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"time"

	"ssh-manager-mcp/internal/vaultio"
)

// errSnapshotVersionNotFound 表示权威端没有 /snapshot/version 接口(旧版
// 权威端返回 404)。调用方据此回到原来的「到期直接全量拉取」。
var errSnapshotVersionNotFound = errors.New("authority has no snapshot version endpoint")

// snapshotVersionResponse 是修订号查询接口的响应体,只有一个字段。
type snapshotVersionResponse struct {
	SnapshotRevision int64 `json:"snapshot_revision"`
}

// querySnapshotVersion 调用权威端的 GET /snapshot/version,返回当前快照
// 修订号与响应里的权威端时间。请求头复用统一的设备码与客户端版本设置
// 函数。pin 必须非空:自动保鲜只走锚定 TLS 连接。
func querySnapshotVersion(baseURL, deviceCode, pin string, timeout time.Duration) (int64, time.Time, error) {
	if pin == "" {
		return 0, time.Time{}, fmt.Errorf("snapshot version query has no server pin; refusing plaintext query")
	}
	if u, err := neturl.Parse(baseURL); err != nil || u.Scheme != "https" {
		return 0, time.Time{}, fmt.Errorf("snapshot version query requires an https authority URL (got %q)", baseURL)
	}
	transport, err := pinningTransport(pin)
	if err != nil {
		return 0, time.Time{}, err
	}
	client := &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	if timeout > 0 {
		client.Timeout = timeout
	}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, baseURL+"/snapshot/version", nil)
	if err != nil {
		return 0, time.Time{}, err
	}
	setAuthorityRequestHeaders(req, deviceCode)
	res, err := client.Do(req)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("query snapshot revision: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return 0, time.Time{}, fmt.Errorf("query snapshot revision: %w", errSnapshotVersionNotFound)
	}
	if res.StatusCode != http.StatusOK {
		return 0, time.Time{}, fmt.Errorf("query snapshot revision: server returned %d", res.StatusCode)
	}
	var body snapshotVersionResponse
	if err := json.NewDecoder(io.LimitReader(res.Body, 8<<10)).Decode(&body); err != nil {
		return 0, time.Time{}, fmt.Errorf("query snapshot revision: decode response: %w", err)
	}
	if body.SnapshotRevision <= 0 {
		return 0, time.Time{}, fmt.Errorf("query snapshot revision: server returned invalid revision %d", body.SnapshotRevision)
	}
	serverTime, err := http.ParseTime(res.Header.Get("Date"))
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("query snapshot revision succeeded but the response has no valid Date header — refusing to refresh the cache contact time")
	}
	if skew := time.Since(serverTime); skew > cacheSkewTolerance || skew < -cacheSkewTolerance {
		return 0, time.Time{}, fmt.Errorf("query snapshot revision: server clock skew too large (server %s vs local %s, cap 1h)",
			serverTime.Format(time.RFC3339), time.Now().Format(time.RFC3339))
	}
	return body.SnapshotRevision, serverTime, nil
}

// cachedSnapshotRevision 解密当前实例的加密缓存并读出其中的快照修订号;
// 旧缓存没有该字段时返回 0。第三个返回值为 false 表示 cache.bin 不存在
// (尚未拉取过),调用方应直接走全量拉取。
func cachedSnapshotRevision(instance string) (int64, bool, error) {
	_, bin, _, _, err := CachePathsFor(instance)
	if err != nil {
		return 0, false, err
	}
	blob, err := os.ReadFile(bin)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	dek, err := loadDEK(instance)
	if err != nil {
		return 0, false, err
	}
	plaintext, err := vaultio.DecryptWithKey(dek, blob)
	if err != nil {
		return 0, false, fmt.Errorf("decrypt cached snapshot for revision: %w", err)
	}
	var snap struct {
		SnapshotRevision int64 `json:"snapshot_revision"`
	}
	if err := json.Unmarshal(plaintext, &snap); err != nil {
		return 0, false, fmt.Errorf("decode cached snapshot revision: %w", err)
	}
	return snap.SnapshotRevision, true, nil
}

// refreshCacheContactTime 在不下载全量快照的情况下,把缓存元数据里的最后
// 联系时间推进到权威端时间。离线上限按这个时间计算,所以修订号未变化的
// 到期保鲜也要刷新它;其余元数据字段原样保留。
func refreshCacheContactTime(instance string, serverTime time.Time) error {
	_, _, metaPath, _, err := CachePathsFor(instance)
	if err != nil {
		return err
	}
	meta, err := readCacheMeta(metaPath)
	if err != nil {
		return fmt.Errorf("refresh cache contact time: %w", err)
	}
	meta.PulledAt = serverTime.Unix()
	meta.ServerAnchored = true
	blob, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if err := atomicWriteUnique(metaPath, blob); err != nil {
		return fmt.Errorf("refresh cache contact time: %w", err)
	}
	return nil
}
