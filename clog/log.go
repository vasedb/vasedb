package clog

import (
	"fmt"
	"io"
	"log"
	"os"
	"runtime"

	"github.com/fatih/color"
	"gopkg.in/natefinch/lumberjack.v2"
)

const (
	processName = "WIREDKV"
)

var (
	// Logger colors and log message prefixes
	warnColor   = color.New(color.Bold, color.FgYellow)
	infoColor   = color.New(color.Bold, color.FgGreen)
	redColor    = color.New(color.Bold, color.FgRed)
	debugColor  = color.New(color.Bold, color.FgBlue)
	errorPrefix = redColor.Sprintf("[ERROR]\t")
	warnPrefix  = warnColor.Sprintf("[WARN]\t")
	infoPrefix  = infoColor.Sprintf("[INFO]\t")
	debugPrefix = debugColor.Sprintf("[DEBUG]\t")

	IsDebug = false
)

var (
	clog *log.Logger
	dlog *log.Logger
)

func init() {
	// 总共有两套日志记录器
	// [WIREDKV:C] 为主进程记录器记录正常运行状态日志信息
	// [WIREDKV:D] 为辅助记录器记录为 Debug 模式下的日志信息
	clog = newLogger(os.Stdout, "["+processName+":C] ", log.Ldate|log.Ltime)
	// [WIREDKV:D] 只能输出日志信息到标准输出中
	dlog = newLogger(os.Stdout, "["+processName+":D] ", log.Ldate|log.Ltime|log.Lshortfile)
}

func newLogger(out io.Writer, prefix string, flag int) *log.Logger {
	return log.New(out, prefix, flag)
}

func multipleLogger(out io.Writer, prefix string, flag int) {
	clog = log.New(out, prefix, flag)
}

func SetOutput(path string) {
	// 使用 lumberjack 设置日志轮转
	lumberjackLogger := &lumberjack.Logger{
		Filename:   path,
		MaxSize:    10,   // 每个日志文件最大 10 MB
		MaxBackups: 3,    // 最多保留 3 个备份
		MaxAge:     7,    // 日志文件最多保留 7 天
		Compress:   true, // 启用压缩
	}

	// 正常模式的日志记录需要输出到控制台和日志文件中
	multipleLogger(io.MultiWriter(os.Stdout, lumberjackLogger), "["+processName+":C] ", log.Ldate|log.Ltime)
}

func Error(v ...interface{}) {
	clog.Output(2, errorPrefix+fmt.Sprint(v...))
}

func Errorf(format string, v ...interface{}) {
	clog.Output(2, errorPrefix+fmt.Sprintf(format, v...))
}

func Warn(v ...interface{}) {
	clog.Output(2, warnPrefix+fmt.Sprint(v...))
}

func Warnf(format string, v ...interface{}) {
	clog.Output(2, warnPrefix+fmt.Sprintf(format, v...))
}

func Info(v ...interface{}) {
	clog.Output(2, infoPrefix+fmt.Sprint(v...))
}

func Infof(format string, v ...interface{}) {
	clog.Output(2, infoPrefix+fmt.Sprintf(format, v...))
}

func Debug(v ...interface{}) {
	if IsDebug {
		dlog.Output(2, debugPrefix+fmt.Sprint(v...))
	}
}

func Debugf(format string, v ...interface{}) {
	if IsDebug {
		dlog.Output(2, debugPrefix+fmt.Sprintf(format, v...))
	}
}

func Failed(v ...interface{}) {
	pc, file, line, _ := runtime.Caller(1)
	function := runtime.FuncForPC(pc)
	message := fmt.Sprintf("%s:%d %s() %s", file, line, function.Name(), fmt.Sprint(v...))
	clog.Output(2, errorPrefix+message)
	panic(message)
}

func Failedf(format string, v ...interface{}) {
	pc, file, line, _ := runtime.Caller(1)
	function := runtime.FuncForPC(pc)
	message := fmt.Sprintf("%s:%d %s() %s", file, line, function.Name(), fmt.Sprint(v...))
	// 输出日志并触发 panic
	clog.Output(2, errorPrefix+message)
	panic(message)
}
