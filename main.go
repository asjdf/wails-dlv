package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/samber/lo"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var Version = "dev"

type WailsConfig struct {
	OutputFilename string `json:"outputfilename"`
}

type DlvInstance struct {
	CreatedAt time.Time

	Port int
	//Listener   net.Listener // 和dlv实例连接的dial
	Pid        int // dlv实例依附的pid
	Ctx        context.Context
	CancelFunc context.CancelFunc
}

func (o DlvInstance) String() string {
	return fmt.Sprintf("[%s] pid: %d, port: %d", o.CreatedAt.Format("2006-01-02 15:04:05"), o.Pid, o.Port)
}

func pipe(src, dest net.Conn, onClose func()) {
	errChan := make(chan error, 1)
	go func() {
		defer onClose()
		_, err := io.Copy(src, dest)
		errChan <- err
	}()
	go func() {
		_, err := io.Copy(dest, src)
		errChan <- err
	}()
	<-errChan
}

// 读取 wails.json 文件的 outputfilename 字段
func getOutputFilename(configPath string) (string, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return "", err
	}
	var config WailsConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return "", err
	}
	return filepath.Base(config.OutputFilename), nil
}

func main() {
	fmt.Println("wails-dlv " + Version)
	// 获取命令行参数
	args := os.Args[1:]
	if len(args) == 0 || args[0] != "dev" {
		fmt.Println("暂不支持该命令")
		return
	}

	// 解析 wails.json 文件中的 outputfilename
	outputFilename, err := getOutputFilename("wails.json")
	if err != nil {
		fmt.Printf("无法读取 wails.json 文件: %s\n", err)
		return
	}
	fmt.Printf("监控目标进程名: %s\n", outputFilename)

	listener, err := net.Listen("tcp", ":2345")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer listener.Close()

	// 执行命令
	ctx, cancelFunc := context.WithCancel(context.Background())
	cmd := CommandContext(ctx, "wails", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	//cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // 设置新的进程组

	if err := cmd.Start(); err != nil {
		fmt.Printf("命令执行失败: %s\n", err)
	}
	fmt.Printf("主进程 PID: %d\n", cmd.Process.Pid)

	// 用于存储已发现的子进程 ID，避免重复输出
	knownPIDs := make(map[int]bool)

	var dlvInstMap = map[int]*DlvInstance{} // pid:instance
	var dlvInstMu sync.Mutex
	// 协程循环监听新创建的子进程
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
				// 获取符合 outputFilename 的子进程 ID
				childProcesses := findSpecificChildProcesses(cmd.Process.Pid, outputFilename)
				for pid, args := range childProcesses {
					if !knownPIDs[pid] {
						knownPIDs[pid] = true
						fmt.Printf("符合条件的子进程 PID: %d, 启动命令: %s\n", pid, args)

						//// 关闭其他dlv实例
						//for _, instance := range dlvInstMap {
						//	instance.CancelFunc()
						//}

						go func(pid int) {
							port := rand.IntN(1000) + 60000
							dlvInst := DlvInstance{Pid: pid, CreatedAt: time.Now()}
							dlvInst.Ctx, dlvInst.CancelFunc = context.WithCancel(context.Background())
							dlvCmdField := strings.Fields(fmt.Sprintf("dlv attach --accept-multiclient --headless --continue --check-go-version=false --only-same-user=false --listen=:%d %d", port, pid))
							dlvCmd := CommandContext(dlvInst.Ctx, dlvCmdField[0], dlvCmdField[1:]...)
							dlvCmd.Stdout = os.Stdout
							dlvCmd.Stderr = os.Stderr
							dlvInst.Port = port
							dlvInstMu.Lock()
							dlvInstMap[pid] = &dlvInst
							dlvInstMu.Unlock()
							fmt.Printf("创建pid为%d的dlv实例成功\n", pid)
							go func() {
								if err := dlvCmd.Run(); err != nil {
									fmt.Printf("命令执行结束: %s\n", err)
								}
								dlvInstMu.Lock()
								//_ = syscall.Kill(dlvInst.Pid, syscall.SIGKILL)
								delete(dlvInstMap, pid)
								dlvInstMu.Unlock()
								fmt.Printf("pid为%d的dlv实例销毁完成\n", pid)
							}()
						}(pid)
					}
				}
			}
		}
	}()

	go func() {
		var currentInstance *DlvInstance
		var dlvConn net.Conn

		for {
			fmt.Println(currentInstance)
			conn, err := listener.Accept()
			if err != nil {
				fmt.Println(err)
				return
			}
			fmt.Println("收到dlv连接请求")

			go func() {
				// 处理连接
				// 分别读取client发来的数据和dlv发来的数据
				// 如果dlv连接为空，则创建一个
				if dlvConn == nil {
					// 从dlv实例列表找到一个dlv实例并连接
					for _, instance := range dlvInstMap {
						fmt.Printf("尝试dlv连接pid为%d的实例\n", instance.Pid)
						tmpConn, err := net.Dial("tcp", fmt.Sprintf(":%d", instance.Port))
						if err != nil {
							fmt.Printf("pid为%d的dlv实例连接失败，错误为%s\n", instance.Pid, err)
							continue
						}
						dlvConn = tmpConn
						currentInstance = instance
						fmt.Println("dlv连接成功")

						go pipe(conn, dlvConn, func() {
							dlvConn = nil
							fmt.Printf("断开与pid为%d的dlv的连接\n", currentInstance.Pid)
						})
						break
					}
					if dlvConn == nil {
						fmt.Println("无可用dlv实例")
						return
					}
				}
			}()
		}
	}()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(1 * time.Second):
				if time.Now().Unix()%10 == 0 {
					fmt.Println("当前wails子进程:", dlvInstMap)
				}

				dlvInstList := lo.MapToSlice(dlvInstMap, func(k int, v *DlvInstance) *DlvInstance {
					return v
				})
				if len(dlvInstList) > 1 {
					sort.Slice(dlvInstList, func(i, j int) bool { return dlvInstList[i].CreatedAt.After(dlvInstList[j].CreatedAt) })
					fmt.Println("已创建的dlv实例", dlvInstList)
					for _, instance := range lo.Filter(dlvInstList, func(instance *DlvInstance, _ int) bool {
						return instance.CreatedAt.Before(time.Now().Add(-2 * time.Second))
					})[1:] {
						fmt.Println("销毁实例", instance.String())
						_ = syscall.Kill(instance.Pid, syscall.SIGKILL)
						//dlvInstList = lo.Filter(dlvInstList, func(instance *DlvInstance, _ int) bool { return instance != instance })
						dlvInstMu.Lock()
						//_ = syscall.Kill(dlvInst.Pid, syscall.SIGKILL)
						delete(dlvInstMap, instance.Pid)
						dlvInstMu.Unlock()
						fmt.Printf("pid为%d的dlv实例销毁完成\n", instance.Pid)

					}
				}
			}
		}
	}()

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)

	// 在协程中等待主进程退出
	go func() {
		// 阻塞等待主进程结束
		err := cmd.Wait()
		if err != nil {
			fmt.Printf("主进程退出错误: %s\n", err)
		} else {
			fmt.Println("主进程已正常退出")
		}
		ch <- syscall.SIGTERM
	}()

	<-ch
	cancelFunc()
	// 使用负的 PID 发送信号，以便终止整个进程组
	for _, instance := range dlvInstMap {
		_ = syscall.Kill(instance.Pid, syscall.SIGTERM)
	}
}

// 查找符合 outputFilename 的子进程
func findSpecificChildProcesses(parentPID int, outputFilename string) map[int]string {
	output, err := exec.Command("ps", "-o", "ppid,pid,args").Output()
	if err != nil {
		fmt.Printf("无法执行 ps 命令: %s\n", err)
		return nil
	}

	matchedProcesses := make(map[int]string)
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}

		ppid := fields[0]
		pid := fields[1]
		comm := fields[2]
		args := strings.Join(fields[3:], " ")

		// 检查是否符合指定的父进程 ID，并匹配 outputFilename
		if ppid == fmt.Sprint(parentPID) && path.Base(comm) == outputFilename {
			pidInt, err := strconv.Atoi(pid)
			if err == nil {
				matchedProcesses[pidInt] = args
			}
		}
	}
	return matchedProcesses
}
