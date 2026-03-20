package log_helper

import (
	"sort"

	"github.com/sirupsen/logrus"
	"gopkg.in/natefinch/lumberjack.v2"
)

var logHelper *logrus.Logger

// 初始化日志助手
func InitlogHelper() {
	logHelper = logrus.New()
	// 设置日志级别为 Info
	logHelper.SetLevel(logrus.InfoLevel)
	//设置日志格式
	logHelper.SetFormatter(&logrus.TextFormatter{
		TimestampFormat: "2006-01-02 15:04:05.000",
		FullTimestamp:   true,
		DisableColors:   true,
		FieldMap: logrus.FieldMap{
			logrus.FieldKeyTime:  "time",
			logrus.FieldKeyLevel: "level",
			logrus.FieldKeyMsg:   "msg",
		},
		SortingFunc: func(keys []string) {
			order := map[string]int{
				"time":     0,
				"level":    1,
				"msg":      2,
				"status":   3,
				"method":   4,
				"path":     5,
				"rule":     6,
				"identity": 7,
				"success":  8,
				"pending":  9,
				"elapsed":  10,
				"error":    11,
				"panic":    12,
			}
			sort.Slice(keys, func(i, j int) bool {
				left, lok := order[keys[i]]
				right, rok := order[keys[j]]
				if lok && rok {
					return left < right
				}
				if lok {
					return true
				}
				if rok {
					return false
				}
				return keys[i] < keys[j]
			})
		},
	})
	// 创建一个新的 lumberjack.Logger 实例
	logFilePath := "./runtime/logs/app.log"
	hook := &lumberjack.Logger{
		Filename:   logFilePath,
		MaxSize:    50,  // 单位：MB
		MaxAge:     365, // 保留时间：天
		MaxBackups: 100, // 最大备份数量
	}

	// 设置日志输出到 hook
	logHelper.SetOutput(hook)
}

func fields(args ...interface{}) logrus.Fields {
	result := logrus.Fields{}
	for i := 0; i+1 < len(args); i += 2 {
		key, ok := args[i].(string)
		if !ok {
			continue
		}
		result[key] = args[i+1]
	}
	return result
}

// 写日志
func Info(message string, args ...interface{}) {
	logHelper.WithFields(fields(args...)).Info(message)
}
func Error(message string, args ...interface{}) {
	logHelper.WithFields(fields(args...)).Error(message)
}
func Warning(message string, args ...interface{}) {
	logHelper.WithFields(fields(args...)).Warning(message)
}
func Debug(message string, args ...interface{}) {
	logHelper.WithFields(fields(args...)).Debug(message)
}
func Fatal(message string, args ...interface{}) {
	logHelper.WithFields(fields(args...)).Fatal(message)
}
