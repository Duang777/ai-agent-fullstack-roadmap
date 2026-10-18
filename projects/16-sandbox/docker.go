package sandbox

import (
	"fmt"
	"strconv"
)

// ContainerSpec 描述一次“用一次就扔”的容器执行。
type ContainerSpec struct {
	Image     string
	Runtime   string // "" 用默认的 runc；"runsc" 是 gVisor
	MemoryMB  int
	CPUs      float64
	PidsLimit int
	Network   bool // 默认不给网络
	Workdir   string
	Code      string
}

// DockerArgs 生成 docker run 参数。每一项都是在收窄攻击面，缺一项就多一个口子。
func DockerArgs(s ContainerSpec) []string {
	args := []string{"run", "--rm", "-i"}
	if s.Runtime != "" {
		args = append(args, "--runtime="+s.Runtime)
	}
	if !s.Network {
		args = append(args, "--network=none")
	}
	args = append(args,
		"--read-only",                               // 根文件系统只读
		"--tmpfs", "/tmp:rw,size=64m,noexec,nosuid", // 只有 /tmp 可写，而且不能执行
		"--cap-drop=ALL",                   // 去掉全部 Linux capabilities
		"--security-opt=no-new-privileges", // setuid 程序也提不了权
		"--user=65534:65534",               // nobody，不用 root
		"--memory="+strconv.Itoa(s.MemoryMB)+"m",
		"--memory-swap="+strconv.Itoa(s.MemoryMB)+"m", // 等于 memory：不许用 swap 绕过内存上限
		"--cpus="+strconv.FormatFloat(s.CPUs, 'f', -1, 64),
		"--pids-limit="+strconv.Itoa(s.PidsLimit), // 防 fork 炸弹
	)
	if s.Workdir != "" {
		args = append(args, "-v", fmt.Sprintf("%s:/work:rw", s.Workdir), "-w", "/work")
	}
	return append(args, s.Image, "sh", "-c", s.Code)
}

// LintDockerArgs 找出危险参数：这些一旦出现，容器基本等于没隔离。
func LintDockerArgs(args []string) []string {
	bad := map[string]string{
		"--privileged":   "特权模式：可以访问宿主机全部设备",
		"--network=host": "共享宿主机网络：能访问宿主机上的所有本地服务",
		"--pid=host":     "共享宿主机进程空间：能看到并发信号给宿主机进程",
		"-v=/var/run/docker.sock:/var/run/docker.sock": "挂载 Docker socket：等于宿主机 root",
		"--security-opt=seccomp=unconfined":            "关闭 seccomp：放开全部系统调用",
	}
	var out []string
	for i, a := range args {
		if a == "-v" && i+1 < len(args) {
			a = "-v=" + args[i+1]
		}
		if why, ok := bad[a]; ok {
			out = append(out, a+"："+why)
		}
	}
	return out
}
