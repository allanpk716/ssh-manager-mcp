// Package switchstate 保存 serve 功能开关的共享解析规则。命令行旗标、环境
// 变量、设置存储与默认值都必须得到同一个结果，运维设置页也用它判断当前
// 生效值与是否被覆盖。
package switchstate

// State 表示三层显式输入中的一层：未设置、开启或关闭。
type State int

const (
	Unset State = iota
	On
	Off
)

// Parse 只接受精确的 true 与 false；空值或其它写法都按未设置处理，继续看
// 下一层。
func Parse(value string) State {
	switch value {
	case "true":
		return On
	case "false":
		return Off
	default:
		return Unset
	}
}

// Resolve 按环境变量、命令行旗标、设置存储、默认值的顺序解析开关。
func Resolve(envValue string, flagSet bool, flagValue bool, storedValue string, defaultValue bool) bool {
	if state := Parse(envValue); state != Unset {
		return state == On
	}
	if flagSet {
		return flagValue
	}
	if state := Parse(storedValue); state != Unset {
		return state == On
	}
	return defaultValue
}

// ResolveInputs 解析已经读成可选布尔值的环境变量与旗标。指针为空表示那一
// 层没有显式设置。
func ResolveInputs(envValue, flagValue *bool, storedValue string, defaultValue bool) bool {
	if envValue != nil {
		return *envValue
	}
	if flagValue != nil {
		return *flagValue
	}
	return Resolve("", false, false, storedValue, defaultValue)
}
