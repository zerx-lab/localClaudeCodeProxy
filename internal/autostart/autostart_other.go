//go:build !windows && !darwin

// 其它平台（Linux / *BSD 等）暂不实现开机启动。
// 未来要支持 Linux 可写 ~/.config/autostart/<name>.desktop。

package autostart

func enable(name, exePath string, args []string) error {
	return ErrUnsupported
}

func disable(name string) error {
	return ErrUnsupported
}

func isEnabled(name string) (bool, error) {
	return false, nil
}

func supported() bool { return false }
