// worker/main.go
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/zan8in/afrog/v3"
)

var (
	masterURL         string
	workerID          string
	heartbeatInterval time.Duration
	currentTask       *Task
	isBusy            bool
)

type Worker struct {
	ID      string `json:"id"`
	Address string `json:"address"`
	Busy    bool   `json:"busy"`
}

type Task struct {
	ID      string   `json:"id"`
	Targets []string `json:"targets"`
	Status  string   `json:"status"`
}

func init() {
	flag.StringVar(&masterURL, "master", "", "Master服务器的URL (必填)")
	defaultID, _ := uuid.NewRandom()
	flag.StringVar(&workerID, "id", defaultID.String(), "此worker的唯一ID")
	flag.DurationVar(&heartbeatInterval, "heartbeat", 30*time.Second, "心跳间隔")
	flag.Parse()
}

func main() {
	if masterURL == "" {
		log.Fatal("必须通过 -master 参数指定 Master 服务器地址")
	}

	log.Printf("Worker %s 启动，连接到master：%s", workerID, masterURL)

	// 注册worker
	if err := registerWithMaster(); err != nil {
		log.Fatalf("无法向master注册: %v", err)
	}

	// 定期发送心跳
	go sendHeartbeats()

	// 主循环：获取并执行任务
	for {
		if !isBusy {
			task, err := getTaskFromMaster()
			if err != nil {
				log.Printf("获取任务出错: %v", err)
				time.Sleep(5 * time.Second)
				continue
			}

			if task != nil {
				isBusy = true
				currentTask = task
				go executeTask(task)
			} else {
				// 没有任务，等待一段时间再试
				time.Sleep(5 * time.Second)
			}
		} else {
			time.Sleep(1 * time.Second)
		}
	}
}

// 向master注册worker
func registerWithMaster() error {
	worker := Worker{
		ID:      workerID,
		Address: getWorkerAddress(),
		Busy:    false,
	}

	body, err := json.Marshal(worker)
	if err != nil {
		return fmt.Errorf("序列化worker数据出错: %v", err)
	}

	resp, err := http.Post(fmt.Sprintf("%s/register", masterURL), "application/json", bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("发送注册请求出错: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("master返回非OK状态: %s", resp.Status)
	}

	log.Printf("成功向master注册")
	return nil
}

// 定期向master发送心跳
func sendHeartbeats() {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for range ticker.C {
		worker := Worker{
			ID:      workerID,
			Address: getWorkerAddress(),
			Busy:    isBusy,
		}

		body, err := json.Marshal(worker)
		if err != nil {
			log.Printf("序列化心跳数据出错: %v", err)
			continue
		}

		resp, err := http.Post(fmt.Sprintf("%s/heartbeat", masterURL), "application/json", bytes.NewBuffer(body))
		if err != nil {
			log.Printf("发送心跳出错: %v", err)
			continue
		}
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			log.Printf("master为心跳返回非OK状态: %s", resp.Status)
		}
	}
}

// 从master获取任务
func getTaskFromMaster() (*Task, error) {
	resp, err := http.Get(fmt.Sprintf("%s/task/get?worker_id=%s", masterURL, workerID))
	if err != nil {
		return nil, fmt.Errorf("请求任务出错: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("master返回非OK状态: %s", resp.Status)
	}

	var response struct {
		Status  string `json:"status"`
		Message string `json:"message,omitempty"`
		Task    *Task  `json:"task,omitempty"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, fmt.Errorf("解码响应出错: %v", err)
	}

	if response.Status != "task_assigned" {
		// 没有获取到任务，但不算错误
		log.Printf("没有可用任务: %s", response.Message)
		return nil, nil
	}

	log.Printf("接收到任务 %s，包含 %d 个目标", response.Task.ID, len(response.Task.Targets))
	return response.Task, nil
}

// 执行扫描任务
func executeTask(task *Task) {
	log.Printf("开始执行任务 %s", task.ID)

	// 对每个目标进行扫描
	for _, target := range task.Targets {
		// 从URL中提取IP和端口用于输出文件命名
		parts := strings.Split(strings.TrimPrefix(strings.TrimPrefix(target, "http://"), "https://"), ":")
		var outputFile string

		if len(parts) >= 2 {
			// URL包含端口
			host := parts[0]
			port := strings.Split(parts[1], "/")[0]
			outputFile = fmt.Sprintf("%s_%s.json", host, port)
		} else {
			// URL不包含端口，假设为默认的80/443
			host := parts[0]
			outputFile = fmt.Sprintf("%s_default.json", host)
		}

		log.Printf("扫描目标 %s，输出到 %s", target, outputFile)

		// 调用afrog扫描器
		err := afrog.NewScanner([]string{target}, afrog.Scanner{
			JsonAll: outputFile,
		})

		if err != nil {
			log.Printf("扫描目标 %s 出错: %v", target, err)
		}
	}

	// 报告任务完成
	if err := reportTaskCompletion(task.ID, "completed"); err != nil {
		log.Printf("报告任务完成出错: %v", err)
	}

	isBusy = false
	currentTask = nil
}

// 报告任务完成状态
func reportTaskCompletion(taskID, status string) error {
	data := struct {
		WorkerID string `json:"worker_id"`
		TaskID   string `json:"task_id"`
		Status   string `json:"status"`
	}{
		WorkerID: workerID,
		TaskID:   taskID,
		Status:   status,
	}

	body, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("序列化任务完成数据出错: %v", err)
	}

	resp, err := http.Post(fmt.Sprintf("%s/task/complete", masterURL), "application/json", bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("报告任务完成出错: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("master返回非OK状态: %s", resp.Status)
	}

	log.Printf("成功报告任务 %s 完成", taskID)
	return nil
}

// 获取worker的地址，仅用于显示/调试
func getWorkerAddress() string {
	hostname, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return hostname
}
