package app

import (
	"errors"
	"net/http"
	"strconv"
)

// 分页仅用于已登录展示查询，不改变来源投递任务和重试队列的取数规则。
func pageRequest(r *http.Request) (int, int, bool, error) {
	explicit := r.URL.Query().Has("page") || r.URL.Query().Has("page_size")
	if !explicit {
		return 1, 100, false, nil
	}
	page, size := 1, 20
	for key, target := range map[string]*int{"page": &page, "page_size": &size} {
		if value := r.URL.Query().Get(key); value != "" {
			n, e := strconv.Atoi(value)
			if e != nil {
				return 0, 0, true, errors.New("分页参数无效")
			}
			*target = n
		}
	}
	if page < 1 || page > 1000000 || size < 1 || size > 100 {
		return 0, 0, true, errors.New("分页参数超出范围")
	}
	return page, size, true, nil
}
