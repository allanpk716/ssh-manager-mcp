// Package secretfill 提供上传内容前的机密项占位符填值。调用方负责先完成
// profile 授权检查；本包只判定机密项自己的可写入服务器列表。
package secretfill

import (
	"bytes"
	"context"
	"errors"
	"fmt"
)

var (
	placeholderPrefix = []byte("{{sshmgr-secret:")
	placeholderSuffix = []byte("}}")
)

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
	// ProblemMissingOrNotAllowed 合并表示名字不存在与目标服务器不在可写
	// 入列表内，避免泄露快照裁剪边界。
	ProblemMissingOrNotAllowed ProblemKind = iota
	// ProblemReadFailed 表示机密项值读取或解密失败。
	ProblemReadFailed
)

// Problem 是一条不携带机密值的填值问题。Offset 是占位符在原始内容中的
// 字节位置，后续格式校验可以复用同一输出形态。
type Problem struct {
	Kind    ProblemKind
	Name    string
	Offset  int
	Message string
}

func (p Problem) Error() string { return p.Message }

// Result 是填值结果。Names 按首次出现顺序去重；有 Problem 时 Content 为
// 空，调用方不得把任何部分写出或返回。
type Result struct {
	Content  []byte
	Names    []string
	Problems []Problem
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
	name   string
	offset int
}

// Fill 解析 content 中严格匹配的机密项占位符，向取值来源取得当前值，并
// 判断 target 是否在每个被引用机密项的可写入服务器列表内。
func Fill(ctx context.Context, content []byte, target ServerRef, source Source) Result {
	references := validReferences(content)
	result := Result{Names: referenceNames(references)}
	if len(references) == 0 {
		result.Content = append([]byte(nil), content...)
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
			result.Problems = append(result.Problems, missingOrNotAllowed(item, target))
		}
	}
	if len(result.Problems) > 0 {
		return result
	}

	filled := make([]byte, 0, len(content))
	for offset := 0; offset < len(content); {
		relative := bytes.Index(content[offset:], placeholderPrefix)
		if relative < 0 {
			filled = append(filled, content[offset:]...)
			break
		}
		start := offset + relative
		filled = append(filled, content[offset:start]...)
		name, end, valid := placeholderAt(content, start)
		if !valid {
			// 非严格占位符保持原样；格式问题的整体拒绝由后续票补齐。
			if end <= start+len(placeholderPrefix) {
				filled = append(filled, placeholderPrefix...)
				offset = end
				continue
			}
			filled = append(filled, content[start:end]...)
			offset = end
			continue
		}
		filled = append(filled, sourceResult.Secrets[name].Value...)
		offset = end
	}
	result.Content = filled
	return result
}

func validReferences(content []byte) []reference {
	var references []reference
	for offset := 0; offset < len(content); {
		relative := bytes.Index(content[offset:], placeholderPrefix)
		if relative < 0 {
			break
		}
		start := offset + relative
		name, end, valid := placeholderAt(content, start)
		if !valid {
			offset = start + len(placeholderPrefix)
			continue
		}
		references = append(references, reference{name: name, offset: start})
		offset = end
	}
	return references
}

func placeholderAt(content []byte, start int) (string, int, bool) {
	afterPrefix := content[start+len(placeholderPrefix):]
	end := bytes.Index(afterPrefix, placeholderSuffix)
	if end < 0 {
		return "", start + len(placeholderPrefix), false
	}
	name := string(afterPrefix[:end])
	if !ValidName(name) {
		return name, start + len(placeholderPrefix) + len(name) + len(placeholderSuffix), false
	}
	return name, start + len(placeholderPrefix) + len(name) + len(placeholderSuffix), true
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

func missingOrNotAllowed(item reference, target ServerRef) Problem {
	return Problem{
		Kind:    ProblemMissingOrNotAllowed,
		Name:    item.name,
		Offset:  item.offset,
		Message: fmt.Sprintf("机密项 %s 不存在，或不允许写入服务器 %s", item.name, targetLabel(target)),
	}
}

func readFailure(item reference, target ServerRef) Problem {
	return Problem{
		Kind:    ProblemReadFailed,
		Name:    item.name,
		Offset:  item.offset,
		Message: fmt.Sprintf("机密项 %s 读取失败；不能写入服务器 %s", item.name, targetLabel(target)),
	}
}
