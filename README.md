<p align="center">
  <a href="http://afrog.net"><img src="https://github.com/zan8in/afrog/raw/main/images/afrog-logo.svg" width="60px" alt="afrog"></a>
</p>
<h1 align="center">hahafrog(分布式afrog)</h1>


<h4 align="center">致敬青蛙</h4>


---



**整体架构**

```
                   +-----------------+
                   |                 |
                   |  Redis Server   |  <-- 任务队列和Worker状态管理
                   |                 |
                   +--------+--------+
                            |
                            |
                   +--------v--------+
                   |                 |
                   |  Master Server  |  <-- 读取任务列表（默认web.txt）、分发任务
                   |                 |
                   +--------+--------+
                            |
                            |
         +------------------+------------------+
         |                  |                  |
+--------v-------+  +-------v--------+  +-----v----------+
|                |  |                |  |                |
|    Worker 1    |  |    Worker 2    |  |    Worker 3    |  <-- 执行扫描任务
|                |  |                |  |                |
+----------------+  +----------------+  +----------------+
```

**前置要求**

1. Redis服务器
2. Go语言环境（如果需要自己编译）
3. 适当的网络连接，确保master和worker之间可以通信

启动步骤

1. 启动Redis服务器 Redis密码自行设置
```bash
redis-server         
```


2. 启动Master服务器，如果不需要自己编译忽略第二步，直接下载编译后文件
```bash
cd master
go build -o hahafrog_master
./hahafrog_master --redis localhost:6379 --redis-pass mypassword --listen :8080 --targets web.txt
```

3. 启动Worker节点，可在多台服务器上执行，目前Windows有BUG，建议只在Linux上运行，如果不需要自己编译忽略第二步，直接下载编译后文件
```bash
cd worker
go build -o hahafrog_worker
./hahafrog_worker --master http://master-ip:8080 --id worker1
```

启动worker节点后即可正常开始扫描。以下是一些API信息

使用API添加新任务:
```bash
curl -X POST http://master-ip:8080/tasks/add -H "Content-Type: application/json" -d '{"targets":["http://example.com"]}'
```

重新加载目标文件:
```bash
curl -X GET http://master-ip:8080/reload
```

## 需要关注的地方

```
可扩展性: 可以根据需要动态添加更多worker节点

容错性: worker故障不会影响整体系统运行

任务动态分配: master根据worker状态动态分配任务

结果独立存储: 每个worker将结果以"ip_port.json"格式保存

Redis支持: 使用Redis作为任务队列，确保任务不会丢失
```

目前没有把所有结果发给master统一存储，为了简便直接是每个worker自己存储任务结果，在worker目录下，只要扫出结果会以ip_port.json格式进行结果存储
