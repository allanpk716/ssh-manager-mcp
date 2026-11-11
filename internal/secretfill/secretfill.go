// Package secretfill 提供上传内容前的机密项占位符填值。调用方负责先完成
// profile 授权检查；本包只判定机密项自己的可写入服务器列表。
package secretfill

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// PlaceholderPrefix 是上传内容中机密项占位符的固定前缀。
const PlaceholderPrefix = "{{sshmgr-secret:"

var (
	placeholderPrefix = []byte(PlaceholderPrefix)
	placeholderSuffix = []byte("}}")
)

// ErrAuthorityTooOld 表示客户端缓存来自不支持下发机密项的旧权威端；调用
// 方不得继续把它误报成机密项名字不存在。
var ErrAuthorityTooOld = errors.New("权威端版本过旧，不支持机密项，请先升级权威端")

// ServerRef 是授权判定需要的服务器条目标识。Name 只用于生成不含机密值的
// 诊断文本，可以为空；ID 必须是 vault 或缓存快照里的服务器条目标识。
type ServerRef struct {
	ID   string
	Name string
}

// Secret 是一个机密项的当前值与可写入服务器列表。Value 只允许留在进程
// 内存中，绝不能序列化到工具结果、日志或审计。
type Secret struct {
	Name            string
	Value           []byte
	WritableServers []ServerRef
}

// SourceResult 是一次取值结果。ReadFailedNames 记录无法解密的机密项名
// 字；数据库整体读取失败由 Secrets 的错误返回值表示。
type SourceResult struct {
	Secrets         map[string]Secret
	ReadFailedNames []string
}

// Source 是机密项取值来源。单机部署与多机客户端缓存可以分别提供实现，
// 填值逻辑只依赖这个接口。
type Source interface {
	Secrets(ctx context.Context, names []string) (SourceResult, error)
}

// ProblemKind 标明填值问题类别，后续整体校验可以把多类问题合并返回。
type ProblemKind int

const (
	// ProblemInvalidFormat 表示内容中出现了以机密项占位符前缀开头、但不
	// 符合严格语法的原文片段。
	ProblemInvalidFormat ProblemKind = iota
	// ProblemMissingOrNotAllowed 合并表示名字不存在与目标服务器不在可写
	// 入列表内，避免泄露快照裁剪边界。
	ProblemMissingOrNotAllowed
	// ProblemReadFailed 表示机密项值读取或解密失败。
	ProblemReadFailed
)

// maxInvalidSnippetBytes 限制格式错误诊断保留的原文片段长度；省略号另计。
const maxInvalidSnippetBytes = 64

// Problem 是一条不携带机密值的填值问题。Offset 是占位符在原始内容中的
// 字节位置，后续格式校验可以复用同一输出形态。
type Problem struct {
	Kind    ProblemKind
	Name    string
	Offset  int
	Line    int
	Snippet string
	Message string
}

func (p Problem) Error() string { return p.Message }

// Result 是填值结果。Names 按首次出现顺序去重；有 Problem 时 Content 为
// 空，调用方不得把任何部分写出或返回。ProjectedContent 只供调用方做连
// 接前的大小预检：有未解析占位符时保留原文，已读到的值只留在进程内。
type Result struct {
	Content          []byte
	Names            []string
	Problems         []Problem
	ProjectedContent []byte
}

// Err 把全部问题合成一个错误；错误文本只来自 Problem.Message。
func (r Result) Err() error {
	if len(r.Problems) == 0 {
		return nil
	}
	errs := make([]error, 0, len(r.Problems))
	for _, problem := range r.Problems {
		errs = append(errs, problem)
	}
	return errors.Join(errs...)
}

type reference struct {
	name    string
	offset  int
	snippet []byte
}

// Fill 解析 content 中严格匹配的机密项占位符，向取值来源取得当前值，并
// 判断 target 是否在每个被引用机密项的可写入服务器列表内。
func Fill(ctx context.Context, content []byte, target ServerRef, source Source) Result {
	references, invalidReferences := scanReferences(content)
	result := Result{Names: referenceNames(references)}
	result.ProjectedContent = append([]byte(nil), content...)
	for _, item := range invalidReferences {
		result.Problems = append(result.Problems, invalidFormat(content, item))
	}
	if len(references) == 0 {
		if len(result.Problems) == 0 {
			result.Content = append([]byte(nil), content...)
		}
		return result
	}
	if source == nil {
		for _, item := range references {
			result.Problems = append(result.Problems, readFailure(item, target))
		}
		return result
	}
	if err := ctx.Err(); err != nil {
		for _, item := range references {
			result.Problems = append(result.Problems, readFailure(item, target))
		}
		return result
	}

	sourceResult, err := source.Secrets(ctx, result.Names)
	if err != nil {
		for _, item := range references {
			result.Problems = append(result.Problems, readFailure(item, target))
		}
		return result
	}
	readFailed := make(map[string]bool, len(sourceResult.ReadFailedNames))
	for _, name := range sourceResult.ReadFailedNames {
		readFailed[name] = true
	}
	for _, item := range references {
		switch {
		case readFailed[item.name]:
			result.Problems = append(result.Problems, readFailure(item, target))
			continue
		}
		secret, exists := sourceResult.Secrets[item.name]
		if !exists || !serverAllowed(secret.WritableServers, target.ID) {
			result.Problems = append(result.Problems, missingOrNotAllowed(item, target, secret, exists))
		}
	}

	result.ProjectedContent = substituteAvailableValues(content, references, sourceResult.Secrets, readFailed)
	if len(result.Problems) > 0 {
		return result
	}

	result.Content = append([]byte(nil), result.ProjectedContent...)
	return result
}

func substituteAvailableValues(content []byte, references []reference, secrets map[string]Secret, readFailed map[string]bool) []byte {
	projected := make([]byte, 0, len(content))
	offset := 0
	for _, item := range references {
		projected = append(projected, content[offset:item.offset]...)
		secret, exists := secrets[item.name]
		if exists && !readFailed[item.name] {
			projected = append(projected, secret.Value...)
		} else {
			projected = append(projected, item.snippet...)
		}
		offset = placeholderEnd(item)
	}
	projected = append(projected, content[offset:]...)
	return projected
}

func placeholderEnd(item reference) int {
	return item.offset + len(placeholderPrefix) + len(item.name) + len(placeholderSuffix)
}

func scanReferences(content []byte) ([]reference, []reference) {
	var references, invalidReferences []reference
	for offset := 0; offset < len(content); {
		relative := bytes.Index(content[offset:], placeholderPrefix)
		if relative < 0 {
			break
		}
		start := offset + relative
		afterPrefix := content[start+len(placeholderPrefix):]
		suffixRelative := bytes.Index(afterPrefix, placeholderSuffix)
		nextPrefixRelative := bytes.Index(afterPrefix, placeholderPrefix)
		if suffixRelative >= 0 && (nextPrefixRelative < 0 || suffixRelative < nextPrefixRelative) {
			nameEnd := start + len(placeholderPrefix) + suffixRelative
			end := nameEnd + len(placeholderSuffix)
			name := string(content[start+len(placeholderPrefix) : nameEnd])
			item := reference{name: name, offset: start, snippet: append([]byte(nil), content[start:end]...)}
			if ValidName(name) {
				references = append(references, item)
			} else {
				invalidReferences = append(invalidReferences, item)
			}
			offset = end
			continue
		}

		boundary := len(content)
		if nextPrefixRelative >= 0 {
			boundary = start + len(placeholderPrefix) + nextPrefixRelative
		}
		snippetEnd := boundary
		if newlineRelative := bytes.IndexByte(afterPrefix, '\n'); newlineRelative >= 0 {
			newlineAbsolute := start + len(placeholderPrefix) + newlineRelative
			if newlineAbsolute < snippetEnd {
				snippetEnd = newlineAbsolute
			}
		}
		invalidReferences = append(invalidReferences, reference{
			offset:  start,
			snippet: append([]byte(nil), content[start:snippetEnd]...),
		})
		offset = boundary
	}
	return references, invalidReferences
}

// ValidName 按规格校验占位符名字：大小写字母、数字、点、下划线、连字符。
func ValidName(name string) bool {
	if name == "" {
		return false
	}
	for _, char := range name {
		switch {
		case char >= 'a' && char <= 'z',
			char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9',
			char == '.', char == '_', char == '-':
		default:
			return false
		}
	}
	return true
}

func referenceNames(references []reference) []string {
	names := make([]string, 0, len(references))
	seen := make(map[string]bool, len(references))
	for _, item := range references {
		if seen[item.name] {
			continue
		}
		seen[item.name] = true
		names = append(names, item.name)
	}
	return names
}

func serverAllowed(servers []ServerRef, serverID string) bool {
	for _, server := range servers {
		if server.ID == serverID {
			return true
		}
	}
	return false
}

func targetLabel(target ServerRef) string {
	if target.Name != "" {
		return target.Name
	}
	return target.ID
}

func invalidFormat(content []byte, item reference) Problem {
	line := 1 + bytes.Count(content[:item.offset], []byte{'\n'})
	snippet := boundedSnippet(item.snippet)
	return Problem{
		Kind:    ProblemInvalidFormat,
		Offset:  item.offset,
		Line:    line,
		Snippet: string(snippet),
		Message: fmt.Sprintf(
			"第%d行字节偏移%d：机密项占位符格式错误；原文片段：%q",
			line, item.offset, snippet,
		),
	}
}

func boundedSnippet(snippet []byte) []byte {
	if len(snippet) <= maxInvalidSnippetBytes {
		return snippet
	}
	end := maxInvalidSnippetBytes
	for end > 0 && !utf8.RuneStart(snippet[end]) {
		end--
	}
	bounded := make([]byte, 0, end+len("..."))
	bounded = append(bounded, snippet[:end]...)
	return append(bounded, "..."...)
}

func missingOrNotAllowed(item reference, target ServerRef, secret Secret, exists bool) Problem {
	message := fmt.Sprintf("机密项 %s 不存在，或不允许写入服务器 %s", item.name, targetLabel(target))
	if exists {
		message += "；" + writableServersHint(secret, target)
	}
	return Problem{
		Kind:    ProblemMissingOrNotAllowed,
		Name:    item.name,
		Offset:  item.offset,
		Message: message,
	}
}

func writableServersHint(secret Secret, target ServerRef) string {
	if len(secret.WritableServers) == 0 {
		return fmt.Sprintf("它当前没有可写入服务器。请 owner 到权威端把 %s 加入该机密项的可写入服务器列表", targetLabel(target))
	}
	labels := make([]string, 0, len(secret.WritableServers))
	for _, server := range secret.WritableServers {
		labels = append(labels, targetLabel(server))
	}
	sort.Strings(labels)
	return fmt.Sprintf("它当前可写入服务器：%s。请 owner 到权威端把 %s 加入该机密项的可写入服务器列表",
		strings.Join(labels, "、"), targetLabel(target))
}

func readFailure(item reference, target ServerRef) Problem {
	return Problem{
		Kind:    ProblemReadFailed,
		Name:    item.name,
		Offset:  item.offset,
		Message: fmt.Sprintf("机密项 %s 读取失败；不能写入服务器 %s", item.name, targetLabel(target)),
	}
}
