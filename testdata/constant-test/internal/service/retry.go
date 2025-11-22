package service

import "example.com/constant-test/internal/config"

// DoWithRetry 使用重试机制执行操作
func DoWithRetry() error {
	for i := 0; i < config.MaxRetries; i++ {
		// 执行操作
		if err := performOperation(); err != nil {
			continue
		}
		return nil
	}
	return nil
}

func performOperation() error {
	return nil
}
