// json_helper.go 包内共享的 JSON 工具，避免在多个文件里重复 import "encoding/json"。
package ccproxy

import "encoding/json"

// jsonUnmarshalLoose 是 json.Unmarshal 的包级别名，用来在非 transforms.go 文件里调用，
// 同时显式表达"对未知字段宽松"的意图。
func jsonUnmarshalLoose(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
