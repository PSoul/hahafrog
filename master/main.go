// master/main.go
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
)

var (
	redisAddr   string
	listenAddr  string
	targetFile  string
	redisClient *redis.Client
	ctx         = context.Background()
	workersMu   sync.Mutex
	workers     = make(map[string]*Worker)
)

type Worker struct {
	ID            string    `json:"id"`
	Address       string    `json:"address"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	Busy          bool      `json:"busy"`
}

type Task struct {
	ID      string   `json:"id"`
	Targets []string `json:"targets"`
	Status  string   `json:"status"` // pending, running, completed, failed
}

func init() {
	flag.StringVar(&redisAddr, "redis", "localhost:6379", "Redis server address")
	flag.StringVar(&listenAddr, "listen", ":8080", "Address to listen on")
	flag.StringVar(&targetFile, "targets", "web.txt", "File containing target URLs")
	flag.Parse()

	redisClient = redis.NewClient(&redis.Options{
		Addr: redisAddr,
	})

	// 确保Redis连接正常工作
	if err := redisClient.Ping(ctx).Err(); err != nil {
		log.Fatalf("无法连接到Redis: %v", err)
	}
}

func main() {
	// 从web.txt加载目标
	go loadTargetsFromFile()

	// 定期检查worker心跳
	go checkWorkerHeartbeats()

	// 设置HTTP路由
	http.HandleFunc("/register", handleWorkerRegister)
	http.HandleFunc("/heartbeat", handleWorkerHeartbeat)
	http.HandleFunc("/task/complete", handleTaskComplete)
	http.HandleFunc("/task/get", handleGetTask)
	http.HandleFunc("/tasks/add", handleAddTasks)
	http.HandleFunc("/reload", handleReloadTargets)

	log.Printf("Master服务器启动于 %s", listenAddr)
	if err := http.ListenAndServe(listenAddr, nil); err != nil {
		log.Fatalf("服务器启动失败: %v", err)
	}
}

// 从文件加载目标到任务队列
func loadTargetsFromFile() {
	log.Printf("从%s加载目标", targetFile)

	file, err := os.Open(targetFile)
	if err != nil {
		log.Printf("打开目标文件出错: %v", err)
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	var targets []string
	for scanner.Scan() {
		target := strings.TrimSpace(scanner.Text())
		if target != "" {
			targets = append(targets, target)
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("读取目标文件出错: %v", err)
		return
	}

	log.Printf("已加载 %d 个目标", len(targets))

	// 目标分批，每批最多10个目标
	const batchSize = 10
	var batches [][]string

	for i := 0; i < len(targets); i += batchSize {
		end := i + batchSize
		if end > len(targets) {
			end = len(targets)
		}
		batches = append(batches, targets[i:end])
	}

	// 将每批目标添加为一个任务
	for _, batch := range batches {
		task := Task{
			ID:      fmt.Sprintf("task_%d", time.Now().UnixNano()),
			Targets: batch,
			Status:  "pending",
		}

		taskJSON, err := json.Marshal(task)
		if err != nil {
			log.Printf("序列化任务出错: %v", err)
			continue
		}

		if err := redisClient.RPush(ctx, "tasks:pending", taskJSON).Err(); err != nil {
			log.Printf("将任务推送到Redis出错: %v", err)
		}
	}
}

// 处理worker注册请求
func handleWorkerRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
		return
	}

	var worker Worker
	if err := json.NewDecoder(r.Body).Decode(&worker); err != nil {
		http.Error(w, "请求体无效", http.StatusBadRequest)
		return
	}

	if worker.ID == "" || worker.Address == "" {
		http.Error(w, "Worker ID和Address是必需的", http.StatusBadRequest)
		return
	}

	worker.LastHeartbeat = time.Now()
	worker.Busy = false

	workersMu.Lock()
	workers[worker.ID] = &worker
	workersMu.Unlock()

	workerJSON, err := json.Marshal(worker)
	if err != nil {
		log.Printf("序列化worker出错: %v", err)
	} else {
		if err := redisClient.HSet(ctx, "workers", worker.ID, workerJSON).Err(); err != nil {
			log.Printf("将worker保存到Redis出错: %v", err)
		}
	}

	log.Printf("Worker已注册: %s 位于 %s", worker.ID, worker.Address)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "registered"})
}

// 处理worker心跳请求
func handleWorkerHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
		return
	}

	var worker Worker
	if err := json.NewDecoder(r.Body).Decode(&worker); err != nil {
		http.Error(w, "请求体无效", http.StatusBadRequest)
		return
	}

	if worker.ID == "" {
		http.Error(w, "Worker ID是必需的", http.StatusBadRequest)
		return
	}

	workersMu.Lock()
	if existingWorker, found := workers[worker.ID]; found {
		existingWorker.LastHeartbeat = time.Now()
		existingWorker.Busy = worker.Busy
	} else {
		worker.LastHeartbeat = time.Now()
		workers[worker.ID] = &worker
	}
	workersMu.Unlock()

	workerJSON, err := json.Marshal(workers[worker.ID])
	if err != nil {
		log.Printf("序列化worker出错: %v", err)
	} else {
		if err := redisClient.HSet(ctx, "workers", worker.ID, workerJSON).Err(); err != nil {
			log.Printf("在Redis中更新worker出错: %v", err)
		}
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "heartbeat_received"})
}

// 处理任务完成报告
func handleTaskComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
		return
	}

	var request struct {
		WorkerID string `json:"worker_id"`
		TaskID   string `json:"task_id"`
		Status   string `json:"status"` // completed或failed
	}

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "请求体无效", http.StatusBadRequest)
		return
	}

	if request.WorkerID == "" || request.TaskID == "" || request.Status == "" {
		http.Error(w, "WorkerID, TaskID和Status是必需的", http.StatusBadRequest)
		return
	}

	// 更新worker状态
	workersMu.Lock()
	if worker, found := workers[request.WorkerID]; found {
		worker.Busy = false
		worker.LastHeartbeat = time.Now()

		workerJSON, err := json.Marshal(worker)
		if err == nil {
			redisClient.HSet(ctx, "workers", worker.ID, workerJSON)
		}
	}
	workersMu.Unlock()

	// 记录任务完成状态
	taskKey := "tasks:" + request.Status
	redisClient.RPush(ctx, taskKey, request.TaskID)

	log.Printf("任务 %s 被worker %s 标记为 %s", request.TaskID, request.WorkerID, request.Status)

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "task_updated"})
}

// Worker获取任务
func handleGetTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
		return
	}

	workerID := r.URL.Query().Get("worker_id")
	if workerID == "" {
		http.Error(w, "worker_id参数是必需的", http.StatusBadRequest)
		return
	}

	// 检查worker是否已注册
	workersMu.Lock()
	worker, found := workers[workerID]
	if !found {
		workersMu.Unlock()
		http.Error(w, "Worker未注册", http.StatusNotFound)
		return
	}

	// 如果worker正忙，返回空任务
	if worker.Busy {
		workersMu.Unlock()
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "no_task", "message": "Worker正忙"})
		return
	}

	workersMu.Unlock()

	// 从队列中获取任务
	taskJSON, err := redisClient.LPop(ctx, "tasks:pending").Result()
	if err == redis.Nil {
		// 没有待处理的任务
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "no_task", "message": "没有待处理的任务"})
		return
	} else if err != nil {
		log.Printf("从Redis获取任务出错: %v", err)
		http.Error(w, "检索任务出错", http.StatusInternalServerError)
		return
	}

	var task Task
	if err := json.Unmarshal([]byte(taskJSON), &task); err != nil {
		log.Printf("反序列化任务出错: %v", err)
		http.Error(w, "解析任务出错", http.StatusInternalServerError)
		return
	}

	// 标记任务为运行中
	task.Status = "running"
	updatedTaskJSON, _ := json.Marshal(task)
	redisClient.RPush(ctx, "tasks:running", updatedTaskJSON)

	// 标记worker为忙碌状态
	workersMu.Lock()
	worker.Busy = true
	worker.LastHeartbeat = time.Now()
	workerJSON, _ := json.Marshal(worker)
	redisClient.HSet(ctx, "workers", worker.ID, workerJSON)
	workersMu.Unlock()

	log.Printf("将任务 %s 分配给worker %s", task.ID, workerID)

	// 返回任务给worker
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "task_assigned",
		"task":   task,
	})
}

// 添加新任务
func handleAddTasks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
		return
	}

	var request struct {
		Targets []string `json:"targets"`
	}

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "请求体无效", http.StatusBadRequest)
		return
	}

	if len(request.Targets) == 0 {
		http.Error(w, "未提供目标", http.StatusBadRequest)
		return
	}

	// 目标分批，每批最多10个目标
	const batchSize = 10
	var batches [][]string

	for i := 0; i < len(request.Targets); i += batchSize {
		end := i + batchSize
		if end > len(request.Targets) {
			end = len(request.Targets)
		}
		batches = append(batches, request.Targets[i:end])
	}

	// 将每批目标添加为一个任务
	tasksAdded := 0
	for _, batch := range batches {
		task := Task{
			ID:      fmt.Sprintf("task_%d", time.Now().UnixNano()),
			Targets: batch,
			Status:  "pending",
		}

		taskJSON, err := json.Marshal(task)
		if err != nil {
			log.Printf("序列化任务出错: %v", err)
			continue
		}

		if err := redisClient.RPush(ctx, "tasks:pending", taskJSON).Err(); err != nil {
			log.Printf("将任务推送到Redis出错: %v", err)
			continue
		}

		tasksAdded++
	}

	log.Printf("添加了 %d 个新任务", tasksAdded)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "tasks_added",
		"count":  tasksAdded,
	})
}

// 重新加载目标文件
func handleReloadTargets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
		return
	}

	go loadTargetsFromFile()

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "reloading",
		"message": "正在重新加载目标",
	})
}

// 定期检查worker心跳，移除不活跃的worker
func checkWorkerHeartbeats() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()

		workersMu.Lock()
		for id, worker := range workers {
			// 如果worker超过2分钟没有心跳，认为它已下线
			if now.Sub(worker.LastHeartbeat) > 2*time.Minute {
				log.Printf("Worker %s 已超时，正在移除", id)
				delete(workers, id)
				redisClient.HDel(ctx, "workers", id)
			}
		}
		workersMu.Unlock()
	}
}
