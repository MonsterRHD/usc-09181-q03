package service

import "encoding/json"

// clone 返回任意领域对象的深拷贝。领域类型全部为 JSON 友好类型，
// 用序列化往返实现最稳妥，避免读模型泄漏内部可变状态。
func clone[T any](v *T) *T {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // 领域类型均可序列化，出现错误属于编程缺陷
	}
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	return &out
}

// cloneSlice 返回切片的深拷贝。
func cloneSlice[T any](in []*T) []*T {
	if in == nil {
		return nil
	}
	out := make([]*T, len(in))
	for i := range in {
		out[i] = clone(in[i])
	}
	return out
}
