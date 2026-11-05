package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/secretmask"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

// MaskedOutput 是全部 MCP 工具输出结构共享的遮蔽结果字段。masked 列出本次
// 返回里实际由 sshmgr 还原成占位符的机密项名字；只有出现在这个列表里的
// 名字对应的占位符才是遮蔽结果。
type MaskedOutput struct {
	Masked []string `json:"masked,omitempty" jsonschema:"names of secrets whose values were replaced by placeholders in this result"`
}

type maskingCallKey struct{}

type maskingCall struct {
	masker          secretmask.Masker
	currentServerID string
}

type toolStoreKey struct{}

type toolHostKeyStoreKey struct{}

// withMaskingCall 把本次工具调用的遮蔽器放进 context。后续需要跨读取边界或
// 截断前遮蔽的处理层，应通过 MaskingCallFromContext 取出同一个遮蔽器与当前
// 服务器条目标识符；不要另行读取机密项值集合。
func withMaskingCall(ctx context.Context, call maskingCall) context.Context {
	return context.WithValue(ctx, maskingCallKey{}, call)
}

// MaskingCallFromContext 返回统一工具包装层为本次调用构建的遮蔽器和当前
// 服务器条目标识符。第二个返回值为 false 表示处理器不在 MCP 工具包装层内。
func MaskingCallFromContext(ctx context.Context) (secretmask.Masker, string, bool) {
	call, ok := ctx.Value(maskingCallKey{}).(maskingCall)
	return call.masker, call.currentServerID, ok
}

// readLimitForMasking 返回前台输出或下载的读取上限。存在机密项时，在
// 原上限之外多保留最长匹配串的字节数，调用方先整段遮蔽再截回原上限。
func readLimitForMasking(ctx context.Context, outputLimit int64) int64 {
	masker, _, ok := MaskingCallFromContext(ctx)
	if !ok {
		return outputLimit
	}
	return outputLimit + int64(masker.LongestMatchLength())
}

// maskCappedContent 整段遮蔽已多读的内容，再截回输出上限，并把专门处理层
// 遮蔽到的机密项名字交还给输出结构里的 MaskedOutput。
func maskCappedContent(ctx context.Context, content string, outputLimit int64) (string, []string) {
	masker, currentServerID, ok := MaskingCallFromContext(ctx)
	if !ok || masker.LongestMatchLength() == 0 {
		return content, nil
	}
	result := masker.Mask([]byte(content), currentServerID)
	if int64(len(result.Output)) > outputLimit {
		result.Output = result.Output[:outputLimit]
	}
	return string(result.Output), result.MaskedNames
}

// withToolStore 把本次调用从「调用时求值 store 获取函数」取到的同一个
// store 放进 context。工具处理器必须经 storeFromContext 取它，避免同一个
// 调用里再次执行带热刷新副作用的获取函数。
func withToolStore(ctx context.Context, st *store.Store) context.Context {
	return context.WithValue(ctx, toolStoreKey{}, st)
}

func storeFromContext(ctx context.Context, fallback func() *store.Store) *store.Store {
	if st, ok := ctx.Value(toolStoreKey{}).(*store.Store); ok && st != nil {
		return st
	}
	return fallback()
}

func withToolHostKeyStore(ctx context.Context, hk sshbroker.HostKeyStore) context.Context {
	return context.WithValue(ctx, toolHostKeyStoreKey{}, hk)
}

func toolHostKeyStoreFromContext(ctx context.Context) sshbroker.HostKeyStore {
	hk, _ := ctx.Value(toolHostKeyStoreKey{}).(sshbroker.HostKeyStore)
	return hk
}

func maskToolOutput[In, Out any](storeFn func() *store.Store, hkFn func() sshbroker.HostKeyStore, h mcp.ToolHandlerFor[In, Out]) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		st := storeFn()
		call, err := newMaskingCall(st, in)
		if err != nil {
			var zero Out
			return nil, zero, fmt.Errorf("prepare output masking: %w", err)
		}
		ctx = withMaskingCall(ctx, call)
		ctx = withToolStore(ctx, st)
		hk := sshbroker.HostKeyStore(st)
		if hkFn != nil {
			hk = hkFn()
		}
		ctx = withToolHostKeyStore(ctx, hk)
		result, output, handlerErr := h(ctx, req, in)
		names, maskedErr := maskToolResultAndOutput(result, &output, call, handlerErr)
		if len(names) > 0 && result != nil && reflect.TypeFor[Out]() == reflect.TypeFor[any]() {
			encoded, err := json.Marshal(MaskedOutput{Masked: names})
			if err == nil {
				result.StructuredContent = encoded
			} else {
				return nil, output, fmt.Errorf("encode masked output: %w", err)
			}
		}
		return result, output, maskedErr
	}
}

func withMaskingDescription(tool *mcp.Tool) {
	const sentence = " Only a {{sshmgr-secret:NAME}} placeholder whose NAME appears in the masked field is a masking result."
	tool.Description += sentence
}

func withMaskOnlyOutputSchema(tool *mcp.Tool) {
	tool.OutputSchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"masked": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
			},
		},
		"additionalProperties": false,
	}
}

// maskerFromStore 读取当前 store 的机密项值集合并构建遮蔽器。
func maskerFromStore(st *store.Store) (secretmask.Masker, error) {
	rows, err := st.SecretValuesForMasking()
	if err != nil {
		return secretmask.Masker{}, err
	}
	secrets := make([]secretmask.Secret, 0, len(rows))
	for _, row := range rows {
		secrets = append(secrets, secretmask.Secret{
			Name:              row.Name,
			Current:           row.Current,
			Previous:          row.Previous,
			WritableServerIDs: row.WritableServerIDs,
		})
	}
	return secretmask.New(secrets), nil
}

func newMaskingCall(st *store.Store, input any) (maskingCall, error) {
	masker, err := maskerFromStore(st)
	if err != nil {
		return maskingCall{}, fmt.Errorf("read secret values for masking: %w", err)
	}
	return maskingCall{
		masker:          masker,
		currentServerID: currentServerIDForInput(st, input),
	}, nil
}

func currentServerIDForInput(st *store.Store, input any) string {
	var found string
	foundRank := 0
	walkServerIDFields(reflect.ValueOf(input), func(name, value string) {
		rank := serverIDInputRank(name)
		if value == "" || rank <= foundRank {
			return
		}
		if name == "server" {
			server, err := st.GetServerByName(value)
			if err == nil && server != nil {
				value = server.ID
			}
		}
		found = value
		foundRank = rank
	})
	return found
}

func serverIDInputRank(name string) int {
	switch name {
	case "server_id":
		return 4
	case "to_server_id":
		return 3
	case "from_server_id":
		return 2
	default:
		return 1
	}
}

// shouldVisitStructField 判断结构字段是否参与遍历：非导出字段与 JSON 忽略
// 字段不会进入工具输出。
func shouldVisitStructField(field reflect.StructField) bool {
	return field.PkgPath == "" && strings.Split(field.Tag.Get("json"), ",")[0] != "-"
}

func walkServerIDFields(value reflect.Value, found func(name, value string)) {
	switch value.Kind() {
	case reflect.Pointer:
		if !value.IsNil() {
			walkServerIDFields(value.Elem(), found)
		}
	case reflect.Interface:
		if !value.IsNil() {
			walkServerIDFields(value.Elem(), found)
		}
	case reflect.Struct:
		fieldType := reflect.TypeFor[MaskedOutput]()
		for i := 0; i < value.NumField(); i++ {
			field := value.Type().Field(i)
			jsonName := strings.Split(field.Tag.Get("json"), ",")[0]
			if !shouldVisitStructField(field) || field.Type == fieldType {
				continue
			}
			if field.Type.Kind() == reflect.String && containsServerIDJSONName(jsonName) {
				found(jsonName, value.Field(i).String())
				continue
			}
			walkServerIDFields(value.Field(i), found)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			walkServerIDFields(value.Index(i), found)
		}
	case reflect.Map:
		iter := value.MapRange()
		for iter.Next() {
			walkServerIDFields(iter.Key(), found)
			walkServerIDFields(iter.Value(), found)
		}
	}
}

func containsServerIDJSONName(name string) bool {
	return name == "server_id" || name == "server" || name == "from_server_id" || name == "to_server_id"
}

type maskWalker struct {
	call  maskingCall
	names map[string]bool
	seen  map[uintptr]bool
}

func maskToolResultAndOutput(result *mcp.CallToolResult, output any, call maskingCall, handlerErr error) ([]string, error) {
	if call.masker.LongestMatchLength() == 0 {
		if handlerErr != nil {
			return nil, handlerErr
		}
		return nil, nil
	}
	walker := &maskWalker{call: call, names: make(map[string]bool), seen: make(map[uintptr]bool)}
	maskedErr := handlerErr
	maskedText := ""
	if handlerErr != nil {
		errorResult := call.masker.Mask([]byte(handlerErr.Error()), call.currentServerID)
		for _, name := range errorResult.MaskedNames {
			walker.names[name] = true
		}
		maskedText = string(errorResult.Output)
		maskedErr = errors.New(maskedText)
	}
	if result != nil {
		walker.walk(reflect.ValueOf(result).Elem())
	}
	outputValue := reflect.ValueOf(output)
	walker.walk(outputValue)
	names := make([]string, 0, len(walker.names))
	for name := range walker.names {
		names = append(names, name)
	}
	names = mergeMaskedNames(names)
	if result != nil && result.IsError {
		appendMaskedNamesToErrorText(result, names)
	}
	setMaskedOutputFields(outputValue, names)
	if maskedErr != nil {
		if len(names) == 0 {
			// 没有遮蔽到任何名字时保留原错误，不丢失它的类型。
			maskedErr = handlerErr
		} else {
			maskedErr = maskedErrorMessage(maskedText, names)
		}
	}
	return maskedOutputFieldNames(outputValue), maskedErr
}

func (w *maskWalker) walk(value reflect.Value) {
	if !value.IsValid() {
		return
	}
	switch value.Kind() {
	case reflect.Pointer:
		if value.IsNil() {
			return
		}
		address := value.Pointer()
		if w.seen[address] {
			return
		}
		w.seen[address] = true
		w.walk(value.Elem())
	case reflect.Interface:
		if value.IsNil() {
			return
		}
		element := reflect.New(value.Elem().Type()).Elem()
		element.Set(value.Elem())
		w.walk(element)
		if value.CanSet() {
			value.Set(element)
		}
	case reflect.Map:
		if value.IsNil() {
			return
		}
		replacement := reflect.MakeMapWithSize(value.Type(), value.Len())
		iter := value.MapRange()
		for iter.Next() {
			key := reflect.New(value.Type().Key()).Elem()
			key.Set(iter.Key())
			mapValue := reflect.New(value.Type().Elem()).Elem()
			mapValue.Set(iter.Value())
			w.walk(key)
			w.walk(mapValue)
			replacement.SetMapIndex(key, mapValue)
		}
		if value.CanSet() {
			value.Set(replacement)
		}
	case reflect.Slice:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			w.maskBytes(value)
			return
		}
		for i := 0; i < value.Len(); i++ {
			w.walk(value.Index(i))
		}
	case reflect.Array:
		for i := 0; i < value.Len(); i++ {
			w.walk(value.Index(i))
		}
	case reflect.Struct:
		maskedType := reflect.TypeFor[MaskedOutput]()
		for i := 0; i < value.NumField(); i++ {
			if !shouldVisitStructField(value.Type().Field(i)) {
				continue
			}
			if value.Type().Field(i).Type == maskedType {
				continue
			}
			w.walk(value.Field(i))
		}
	case reflect.String:
		if !value.CanSet() {
			return
		}
		result := w.call.masker.Mask([]byte(value.String()), w.call.currentServerID)
		for _, name := range result.MaskedNames {
			w.names[name] = true
		}
		if len(result.MaskedNames) > 0 {
			value.SetString(string(result.Output))
		}
	}
}

func (w *maskWalker) maskBytes(value reflect.Value) {
	if !value.CanSet() {
		return
	}
	input := value.Bytes()
	result := w.call.masker.Mask(input, w.call.currentServerID)
	for _, name := range result.MaskedNames {
		w.names[name] = true
	}
	if len(result.MaskedNames) > 0 {
		value.SetBytes(result.Output)
	}
}

func appendMaskedNamesToErrorText(result *mcp.CallToolResult, names []string) {
	if result == nil || !result.IsError || len(names) == 0 {
		return
	}
	line := "\nMasked secrets: " + strings.Join(names, ", ")
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			text.Text += line
		}
	}
}

func setMaskedOutputFields(value reflect.Value, names []string) {
	if len(names) == 0 || !value.IsValid() {
		return
	}
	maskedType := reflect.TypeFor[MaskedOutput]()
	switch value.Kind() {
	case reflect.Pointer:
		if !value.IsNil() {
			setMaskedOutputFields(value.Elem(), names)
		}
	case reflect.Interface:
		if !value.IsNil() {
			setMaskedOutputFields(value.Elem(), names)
		}
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			field := value.Type().Field(i)
			if !shouldVisitStructField(field) {
				continue
			}
			if field.Type == maskedType {
				target := value.Field(i)
				if target.CanSet() {
					target.Set(reflect.ValueOf(MaskedOutput{Masked: mergeMaskedNames(target.Field(0).Interface().([]string), names)}))
				}
				continue
			}
			setMaskedOutputFields(value.Field(i), names)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			setMaskedOutputFields(value.Index(i), names)
		}
	case reflect.Map:
		iter := value.MapRange()
		for iter.Next() {
			setMaskedOutputFields(iter.Value(), names)
		}
	}
}

func maskedOutputFieldNames(value reflect.Value) []string {
	if !value.IsValid() {
		return nil
	}
	maskedType := reflect.TypeFor[MaskedOutput]()
	switch value.Kind() {
	case reflect.Pointer:
		if !value.IsNil() {
			return maskedOutputFieldNames(value.Elem())
		}
	case reflect.Interface:
		if !value.IsNil() {
			return maskedOutputFieldNames(value.Elem())
		}
	case reflect.Struct:
		var names []string
		for i := 0; i < value.NumField(); i++ {
			if !shouldVisitStructField(value.Type().Field(i)) {
				continue
			}
			if value.Type().Field(i).Type == maskedType {
				names = append(names, value.Field(i).Field(0).Interface().([]string)...)
				continue
			}
			names = append(names, maskedOutputFieldNames(value.Field(i))...)
		}
		return mergeMaskedNames(names)
	case reflect.Slice, reflect.Array:
		var names []string
		for i := 0; i < value.Len(); i++ {
			names = append(names, maskedOutputFieldNames(value.Index(i))...)
		}
		return mergeMaskedNames(names)
	case reflect.Map:
		var names []string
		iter := value.MapRange()
		for iter.Next() {
			names = append(names, maskedOutputFieldNames(iter.Value())...)
		}
		return mergeMaskedNames(names)
	}
	return nil
}

// mergeMaskedNames 合并多组遮蔽到的机密项名字，按字母顺序去重；没有名字时
// 返回空值。
func mergeMaskedNames(groups ...[]string) []string {
	total := 0
	for _, group := range groups {
		total += len(group)
	}
	if total == 0 {
		return nil
	}
	all := make([]string, 0, total)
	for _, group := range groups {
		all = append(all, group...)
	}
	sort.Strings(all)
	merged := make([]string, 0, len(all))
	for _, name := range all {
		if len(merged) == 0 || name != merged[len(merged)-1] {
			merged = append(merged, name)
		}
	}
	return merged
}

func maskedErrorMessage(text string, names []string) error {
	return errors.New(text + "\nMasked secrets: " + strings.Join(names, ", "))
}
