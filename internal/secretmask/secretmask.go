// Package secretmask 提供与传输方式无关的机密项输出遮蔽逻辑。
package secretmask

import (
	"bytes"
	"sort"
)

// Secret 描述一个参与输出遮蔽的机密项。
type Secret struct {
	Name              string
	Current           []byte
	Previous          []byte
	WritableServerIDs []string
}

// MaskResult 是一次整段遮蔽或一次流式遮蔽取出的结果。
type MaskResult struct {
	// Output 是遮蔽后的字节；没有被遮蔽时保持调用方传入的切片。
	Output []byte
	// MaskedNames 是按字母顺序去重后的机密项名字。
	MaskedNames []string
}

type matchString struct {
	value             []byte
	name              string
	writableServerIDs []string
}

type Masker struct {
	matchStrings []matchString
	longest      int
}

type Stream struct {
	masker          Masker
	currentServerID string
	pending         []byte
}

// New 用机密项集合构造遮蔽器。空值不生成匹配串。
func New(secrets []Secret) Masker {
	var masker Masker
	for _, secret := range secrets {
		for _, value := range [][]byte{secret.Current, secret.Previous} {
			if len(value) == 0 {
				continue
			}
			writableServerIDs := append([]string(nil), secret.WritableServerIDs...)
			matchValue := bytes.Clone(value)
			masker.addMatchString(matchValue, secret.Name, writableServerIDs)
			if bytes.IndexByte(matchValue, '\n') >= 0 {
				lineFeedVersion := bytes.ReplaceAll(matchValue, []byte("\r\n"), []byte("\n"))
				masker.addMatchString(lineFeedVersion, secret.Name, writableServerIDs)
				carriageReturnVersion := bytes.ReplaceAll(lineFeedVersion, []byte("\n"), []byte("\r\n"))
				masker.addMatchString(carriageReturnVersion, secret.Name, writableServerIDs)
			}
		}
	}
	sort.SliceStable(masker.matchStrings, func(i, j int) bool {
		return len(masker.matchStrings[i].value) > len(masker.matchStrings[j].value)
	})
	return masker
}

// NewStream 创建一条输出流的遮蔽状态。currentServerID 为空表示没有当前服务器条目。
func (m Masker) NewStream(currentServerID string) Stream {
	return Stream{masker: m, currentServerID: currentServerID}
}

func (m *Masker) addMatchString(value []byte, name string, writableServerIDs []string) {
	m.matchStrings = append(m.matchStrings, matchString{
		value:             value,
		name:              name,
		writableServerIDs: writableServerIDs,
	})
	if len(value) > m.longest {
		m.longest = len(value)
	}
}

// Mask 整段遮蔽 input，并返回按字母顺序去重后的机密项名字。
func (m Masker) Mask(input []byte, currentServerID string) MaskResult {
	var output []byte
	var names []string
	copied := 0
	for i := 0; i < len(input); {
		selected, matched := m.matchAt(input[i:], currentServerID)
		if !matched {
			i++
		} else {
			output = append(output, input[copied:i]...)
			output = append(output, "{{sshmgr-secret:"+selected.name+"}}"...)
			names = append(names, selected.name)
			i += len(selected.value)
			copied = i
		}
	}
	if len(names) == 0 {
		return MaskResult{Output: input}
	}
	output = append(output, input[copied:]...)
	sort.Strings(names)
	names = deduplicateStrings(names)
	return MaskResult{Output: output, MaskedNames: names}
}

// LongestMatchLength 返回所有匹配串中的最大字节数。
func (m Masker) LongestMatchLength() int {
	return m.longest
}

// Write 写入下一段输出，并立即取出已经可以确定的遮蔽结果。
func (s *Stream) Write(input []byte) MaskResult {
	if len(s.masker.matchStrings) == 0 {
		return MaskResult{Output: input}
	}
	s.pending = append(s.pending, input...)
	return s.takeDecidedOutput()
}

// Flush 结束输出流，取出此前无法确定的尾部字节。
func (s *Stream) Flush() MaskResult {
	if len(s.pending) == 0 {
		return MaskResult{}
	}
	result := s.masker.Mask(s.pending, s.currentServerID)
	s.pending = nil
	return result
}

func (s *Stream) takeDecidedOutput() MaskResult {
	var output []byte
	var names []string
	for len(s.pending) >= s.masker.longest {
		selected, matched := s.masker.matchAt(s.pending, s.currentServerID)
		if matched {
			output = append(output, "{{sshmgr-secret:"+selected.name+"}}"...)
			names = append(names, selected.name)
			s.removeFirst(len(selected.value))
		} else {
			output = append(output, s.pending[0])
			s.removeFirst(1)
		}
	}
	sort.Strings(names)
	names = deduplicateStrings(names)
	return MaskResult{Output: output, MaskedNames: names}
}

func (s *Stream) removeFirst(length int) {
	copy(s.pending, s.pending[length:])
	s.pending = s.pending[:len(s.pending)-length]
}

func (m Masker) matchAt(input []byte, currentServerID string) (matchString, bool) {
	var selected matchString
	found := false
	for _, candidate := range m.matchStrings {
		if bytesEqual(input, candidate.value) {
			if !found || betterMatch(candidate, selected, currentServerID) {
				selected = candidate
			}
			found = true
		}
	}
	return selected, found
}

func betterMatch(candidate matchString, selected matchString, currentServerID string) bool {
	if len(candidate.value) != len(selected.value) {
		return len(candidate.value) > len(selected.value)
	}
	candidateWritable := containsServer(candidate.writableServerIDs, currentServerID)
	selectedWritable := containsServer(selected.writableServerIDs, currentServerID)
	if candidateWritable != selectedWritable {
		return candidateWritable
	}
	return candidate.name < selected.name
}

func containsServer(serverIDs []string, currentServerID string) bool {
	for _, serverID := range serverIDs {
		if serverID == currentServerID {
			return true
		}
	}
	return false
}

func bytesEqual(input []byte, want []byte) bool {
	return len(input) >= len(want) && bytes.Equal(input[:len(want)], want)
}

func deduplicateStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	kept := values[:1]
	for _, value := range values[1:] {
		if value != kept[len(kept)-1] {
			kept = append(kept, value)
		}
	}
	return kept
}
