package logrus

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (

	// qualified package name, cached at first use
	logrusPackage string

	// Positions in the call stack when tracing to report the calling method
	minimumCallerDepth int

	// Used for caller information initialisation
	callerInitOnce sync.Once
)

const (
	maximumCallerDepth int = 25
	knownLogrusFrames  int = 4
)

func init() {
	// start at the bottom of the stack before the package-name cache is primed
	minimumCallerDepth = 1
}

// Defines the key when adding errors using WithError.
var ErrorKey = "error"

// An entry is the final or intermediate Logrus logging entry. It contains all
// the fields passed with WithField{,s}. It's finally logged when Trace, Debug,
// Info, Warn, Error, Fatal or Panic is called on it. These objects can be
// reused and passed around as much as you wish to avoid field duplication.
type Entry struct {
	Logger *Logger

	// Contains all the fields set by the user.
	Data FieldsSlice

	// Contains the context set by the user. Useful for hook processing etc.
	Context context.Context

	// err may contain a field formatting error
	err    string
}

type FieldsSlice []Field

func (s FieldsSlice) ToFields() Fields {
	fields := make(Fields, len(s))
	for _, f := range s {
		fields[f.Key] = f.Value
	}
	return fields
}

func (s FieldsSlice) Get(key string) any {
	for _, f := range s {
		if f.Key == key {
			return f.Value
		}
	}
	return nil
}

type Field struct {
	Key   string
	Value any
}

func NewEntry(logger *Logger) *Entry {
	entry := &Entry{
		Logger: logger,
	}
	return entry
}

func (entry *Entry) Dup() *Entry {
	newEntry := *entry
	entry.Data = make([]Field, len(entry.Data))
	copy(entry.Data, entry.Data)
	return &newEntry
}

func (entry *Entry) dupWithRoom(n int) *Entry {
	newEntry := *entry
	newEntry.Data = make([]Field, len(entry.Data), len(entry.Data)+n)
	copy(newEntry.Data, entry.Data)
	return &newEntry
}

// Returns the bytes representation of this entry from the formatter.
func (entry *Entry) Bytes() ([]byte, error) {
	return entry.Logger.Formatter.Format(entry)
}

// Returns the string representation from the reader and ultimately the
// formatter.
func (entry *Entry) String() (string, error) {
	serialized, err := entry.Bytes()
	if err != nil {
		return "", err
	}
	str := string(serialized)
	return str, nil
}

// Add an error as single field (using the key defined in ErrorKey) to the Entry.
func (entry *Entry) WithError(err error) *Entry {
	return entry.WithField(ErrorKey, err)
}

// Add a context to the Entry.
func (entry *Entry) WithContext(ctx context.Context) *Entry {
	newEntry := entry.Dup()
	newEntry.Context = ctx
	return newEntry
}

// Add a single field to the Entry.
func (entry *Entry) WithField(key string, value interface{}) *Entry {
	newEntry := entry.dupWithRoom(1)
	newEntry.Data = append(newEntry.Data, Field{Key: key, Value: value})
	return newEntry
}

// Add a map of fields to the Entry.
func (entry *Entry) WithFields(fields Fields) *Entry {
	newEntry := entry.dupWithRoom(len(fields))
	newEntry.addFields(fields)
	return newEntry
}

func (entry *Entry) addFields(fields Fields) {
	for k, v := range fields {
		isErrField := false
		if t := reflect.TypeOf(v); t != nil {
			switch {
			case t.Kind() == reflect.Func, t.Kind() == reflect.Ptr && t.Elem().Kind() == reflect.Func:
				isErrField = true
			}
		}
		if isErrField {
			tmp := fmt.Sprintf("can not add field %q", k)
			if entry.err != "" {
				entry.err = entry.err + ", " + tmp
			} else {
				entry.err = tmp
			}
		} else {
			entry.Data = append(entry.Data, Field{Key: k, Value: v})
		}
	}
}

// Overrides the time of the Entry.
func (entry *Entry) WithTime(t time.Time) *Entry {
	return entry
}

// getPackageName reduces a fully qualified function name to the package name
// There really ought to be to be a better way...
func getPackageName(f string) string {
	for {
		lastPeriod := strings.LastIndex(f, ".")
		lastSlash := strings.LastIndex(f, "/")
		if lastPeriod > lastSlash {
			f = f[:lastPeriod]
		} else {
			break
		}
	}

	return f
}

// getCaller retrieves the name of the first non-logrus calling function
func getCaller() (string, int) {
	// cache this package's fully-qualified name
	callerInitOnce.Do(func() {
		pcs := make([]uintptr, maximumCallerDepth)
		_ = runtime.Callers(0, pcs)

		// dynamic get the package name and the minimum caller depth
		for i := 0; i < maximumCallerDepth; i++ {
			funcName := runtime.FuncForPC(pcs[i]).Name()
			if strings.Contains(funcName, "getCaller") {
				logrusPackage = getPackageName(funcName)
				break
			}
		}

		minimumCallerDepth = knownLogrusFrames
	})

	// Restrict the lookback frames to avoid runaway lookups
	var pcs [maximumCallerDepth]uintptr
	depth := runtime.Callers(minimumCallerDepth, pcs[:])
	frames := runtime.CallersFrames(pcs[:depth])
	for f, again := frames.Next(); again; f, again = frames.Next() {
		if strings.HasPrefix(f.Function, logrusPackage) {
			continue
		}
		// If the caller isn't part of this package, we're done
		return path.Base(f.File), f.Line
	}

	// if we got here, we failed to find the caller's context
	return "", 0
}

func (entry *Entry) HasCaller() (has bool) {
	return false
}

func (entry *Entry) log(level Level, msg string, fileName string, lineNo int) {
	const desiredTimeFormat = "2006-01-02 15:04:05.000"
	const desiredTimeLen = len(desiredTimeFormat)

	b := bufferPool.Get()
	b.Grow(desiredTimeLen + 32 + len(fileName) + len(msg) + len(entry.Data)*32)
	{
		buf := b.AvailableBuffer()
		// Want "2006-01-02 15:04:05.000" but the formatter has an optimised
		// impl of RFC3339Nano, which we can easily tweak into our format.
		const tPos = len("2006-01-02T")-1
		buf = time.Now().AppendFormat(buf, time.RFC3339Nano)[:desiredTimeLen]
		buf[tPos] = ' '
		_, _ = b.Write(buf)
	}
	b.WriteString(levelStrings[level])
	//if f.Component != "" {
	//	b.WriteString(f.Component)
	//	b.WriteByte('/')
	//}
	b.WriteString(fileName)
	b.WriteByte(' ')
	b.WriteString(strconv.Itoa(lineNo))
	b.WriteString(": ")
	b.WriteString(msg)
	appendKVsAndNewLine(b, entry.Data)

	entry.Logger.mu.Lock()
	defer entry.Logger.mu.Unlock()
	if _, err := entry.Logger.Out.Write(b.Bytes()); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Failed to write to log, %v\n", err)
	}
	bufferPool.Put(b)

	// To avoid Entry#log() returning a value that only would make sense for
	// panic() to use in Entry#Panic(), we avoid the allocation by checking
	// directly here.
	if level <= PanicLevel {
		panic(entry)
	}
}

func (entry *Entry) getBufferPool() (pool BufferPool) {
	if entry.Logger.BufferPool != nil {
		return entry.Logger.BufferPool
	}
	return bufferPool
}
//
//func (entry *Entry) fireHooks() {
//	var tmpHooks LevelHooks
//	entry.Logger.mu.Lock()
//	tmpHooks = make(LevelHooks, len(entry.Logger.Hooks))
//	for k, v := range entry.Logger.Hooks {
//		tmpHooks[k] = v
//	}
//	entry.Logger.mu.Unlock()
//
//	err := tmpHooks.Fire(entry.Level, entry)
//	if err != nil {
//		fmt.Fprintf(os.Stderr, "Failed to fire hook: %v\n", err)
//	}
//}

func (entry *Entry) write() {
	entry.Logger.mu.Lock()
	defer entry.Logger.mu.Unlock()
	serialized, err := entry.Logger.Formatter.Format(entry)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to obtain reader, %v\n", err)
		return
	}
	if _, err := entry.Logger.Out.Write(serialized); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write to log, %v\n", err)
	}
}

var levelStrings []string

func init() {
	levelStrings = make([]string, len(AllLevels))
	for _, level := range AllLevels {
		levelStrings[level] = fmt.Sprintf(" [%s][%d] ", strings.ToUpper(level.String()), os.Getpid())
	}
}

	const FileNameUnknown = "<nil>"


// appendKeysAndNewLine writes the KV pairs attached to the entry to the end of the buffer, then
// finishes it with a newline.
func appendKVsAndNewLine(b *bytes.Buffer, data FieldsSlice) {
	// Sort the keys for consistent output.  Fixed-size array avoids an alloc
	// if it's big enough.  make() spills if the length is not constant.
	var keysArr [16]string
	keys := keysArr[:0]
	for _, k := range data {
		keys = append(keys, k.Key)
	}
	sort.Strings(keys)

	lastKey := ""
	for _, key := range keys {
		if key == lastKey {
			continue
		}
		lastKey = key
		if key ==  "__flush__" {
			continue
		}
		var value any
		for i := len(data) - 1; i >= 0; i-- {
			if data[i].Key == key {
				value = data[i].Value
				break
			}
		}
		b.WriteByte(' ')
		b.WriteString(key)
		b.WriteByte('=')
		if s, ok := value.(string); ok {
			b.WriteByte(' ')
			b.WriteString(key)
			b.WriteByte('=')
			buf := b.AvailableBuffer()
			buf = strconv.AppendQuote(buf, s)
			b.Write(buf)
			continue
		} else if err, ok := value.(error); ok {
			buf := b.AvailableBuffer()
			buf = strconv.AppendQuote(buf, err.Error())
			b.Write(buf)
		} else if stringer, ok := value.(fmt.Stringer); ok {
			// Trust the value's String() method.
			buf := b.AvailableBuffer()
			buf = strconv.AppendQuote(buf, stringer.String())
			b.Write(buf)
		} else {
			// No string method, use %#v to get a more thorough dump.
			_, _ = fmt.Fprintf(b, "%#v", value)
			continue
		}
	}
	b.WriteByte('\n')
}

// Log will log a message at the level given as parameter.
// Warning: using Log at Panic or Fatal level will not respectively Panic nor Exit.
// For this behaviour Entry.Panic or Entry.Fatal should be used instead.
func (entry *Entry) Log(level Level, args ...interface{}) {
	if entry.Logger.IsLevelEnabled(level) {
		var msg string
		if len(args) == 1 {
			if s, ok := args[0].(string); ok {
				// Mainline case, one argument that is a string: avoid an alloc.
				msg = s
			} else {
				msg = fmt.Sprint(args[0])
			}
		} else {
			msg = fmt.Sprint(args...)
		}

		fileName, lineNo := getCaller()
		entry.log(level, msg, fileName, lineNo)
	}
}

func (entry *Entry) Trace(args ...interface{}) {
	entry.Log(TraceLevel, args...)
}

func (entry *Entry) Debug(args ...interface{}) {
	entry.Log(DebugLevel, args...)
}

func (entry *Entry) Print(args ...interface{}) {
	entry.Info(args...)
}

func (entry *Entry) Info(args ...interface{}) {
	entry.Log(InfoLevel, args...)
}

func (entry *Entry) Warn(args ...interface{}) {
	entry.Log(WarnLevel, args...)
}

func (entry *Entry) Warning(args ...interface{}) {
	entry.Warn(args...)
}

func (entry *Entry) Error(args ...interface{}) {
	entry.Log(ErrorLevel, args...)
}

func (entry *Entry) Fatal(args ...interface{}) {
	entry.Log(FatalLevel, args...)
	entry.Logger.Exit(1)
}

func (entry *Entry) Panic(args ...interface{}) {
	entry.Log(PanicLevel, args...)
}

// Entry Printf family functions

func (entry *Entry) Logf(level Level, format string, args ...interface{}) {
	if entry.Logger.IsLevelEnabled(level) {
		entry.Log(level, fmt.Sprintf(format, args...))
	}
}

func (entry *Entry) Tracef(format string, args ...interface{}) {
	entry.Logf(TraceLevel, format, args...)
}

func (entry *Entry) Debugf(format string, args ...interface{}) {
	entry.Logf(DebugLevel, format, args...)
}

func (entry *Entry) Infof(format string, args ...interface{}) {
	entry.Logf(InfoLevel, format, args...)
}

func (entry *Entry) Printf(format string, args ...interface{}) {
	entry.Infof(format, args...)
}

func (entry *Entry) Warnf(format string, args ...interface{}) {
	entry.Logf(WarnLevel, format, args...)
}

func (entry *Entry) Warningf(format string, args ...interface{}) {
	entry.Warnf(format, args...)
}

func (entry *Entry) Errorf(format string, args ...interface{}) {
	entry.Logf(ErrorLevel, format, args...)
}

func (entry *Entry) Fatalf(format string, args ...interface{}) {
	entry.Logf(FatalLevel, format, args...)
	entry.Logger.Exit(1)
}

func (entry *Entry) Panicf(format string, args ...interface{}) {
	entry.Logf(PanicLevel, format, args...)
}

// Entry Println family functions

func (entry *Entry) Logln(level Level, args ...interface{}) {
	if entry.Logger.IsLevelEnabled(level) {
		entry.Log(level, entry.sprintlnn(args...))
	}
}

func (entry *Entry) Traceln(args ...interface{}) {
	entry.Logln(TraceLevel, args...)
}

func (entry *Entry) Debugln(args ...interface{}) {
	entry.Logln(DebugLevel, args...)
}

func (entry *Entry) Infoln(args ...interface{}) {
	entry.Logln(InfoLevel, args...)
}

func (entry *Entry) Println(args ...interface{}) {
	entry.Infoln(args...)
}

func (entry *Entry) Warnln(args ...interface{}) {
	entry.Logln(WarnLevel, args...)
}

func (entry *Entry) Warningln(args ...interface{}) {
	entry.Warnln(args...)
}

func (entry *Entry) Errorln(args ...interface{}) {
	entry.Logln(ErrorLevel, args...)
}

func (entry *Entry) Fatalln(args ...interface{}) {
	entry.Logln(FatalLevel, args...)
	entry.Logger.Exit(1)
}

func (entry *Entry) Panicln(args ...interface{}) {
	entry.Logln(PanicLevel, args...)
}

// Sprintlnn => Sprint no newline. This is to get the behavior of how
// fmt.Sprintln where spaces are always added between operands, regardless of
// their type. Instead of vendoring the Sprintln implementation to spare a
// string allocation, we do the simplest thing.
func (entry *Entry) sprintlnn(args ...interface{}) string {
	msg := fmt.Sprintln(args...)
	return msg[:len(msg)-1]
}
