// Copyright 2020 Douyu
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package job

import (
	"context"
	"encoding/json"
	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/mvcc/mvccpb"
	"github.com/douyu/juno-agent/pkg/job/etcd"
	"github.com/douyu/juno-agent/util"
	"github.com/douyu/jupiter/pkg/client/etcdv3"
	"github.com/douyu/jupiter/pkg/util/xgo"
	"github.com/douyu/jupiter/pkg/xlog"
	"github.com/sony/sonyflake"
	"strconv"
)

// Node 执行 cron 命令服务的结构体
type worker struct {
	*Config
	*etcdv3.Client
	*Cron

	ID             string
	ImmediatelyRun bool // 是否立即执行

	jobs        Jobs // 和结点相关的任务
	cmds        map[string]*Cmd
	runningJobs map[string]context.CancelFunc

	done      chan struct{}
	taskIdGen *sonyflake.Sonyflake
}

func NewWorker(conf *Config) (w *worker) {
	w = &worker{
		Config:         conf,
		ID:             conf.HostName,
		Client:         etcdv3.StdConfig("default").Build(),
		ImmediatelyRun: false,
		cmds:           make(map[string]*Cmd),
		runningJobs:    make(map[string]context.CancelFunc),
		done:           make(chan struct{}),
		taskIdGen:      sonyflake.NewSonyflake(sonyflake.Settings{}), // default setting
	}

	w.Cron = newCron(w)

	w.logger.Info("agent info :", xlog.String("name", conf.AppIP+":"+conf.HostName))

	return
}

func (w *worker) Run() error {
	w.logger.Info("worker run...")

	w.Cron.Run()
	go w.watchJobs()
	go w.watchOnce()
	go w.watchExecutingProc()

	return nil
}

func (w *worker) loadJobs(keyValue []*mvccpb.KeyValue) {
	count := len(keyValue)
	jobs := make(map[string]*Job, count)
	if count == 0 {
		return
	}

	for _, val := range keyValue {
		job, err := w.GetJobContentFromKv(val.Key, val.Value)
		if err != nil {
			w.logger.Warnf("job[%s] is invalid: %s", val.Key, err.Error())
			continue
		}

		jobs[job.ID] = job
	}

	w.jobs = jobs
	w.logger.Infof("job len : %d", len(w.jobs))
	if len(jobs) == 0 {
		return
	}

	for _, job := range jobs {
		job.runOn = w.ID
		w.addJob(job)
	}

	return
}

// watchJobs watch jobs
func (w *worker) watchJobs() {
	ctx, cancelFunc := NewEtcdTimeoutContext(w)
	defer cancelFunc()

	watch, err := etcd.WatchPrefix(w.Client, ctx, JobsKeyPrefix)
	if err != nil {
		panic(err)
	}

	// 将之前job保存下来
	w.loadJobs(watch.IncipientKeyValues())

	xgo.Go(func() {
		for event := range watch.C() {
			switch {
			case event.IsCreate():
				w.logger.Info("is create..")
				job, err := w.GetJobContentFromKv(event.Kv.Key, event.Kv.Value)
				if err != nil {
					continue
				}

				job.runOn = w.ID
				w.addJob(job)
			case event.IsModify():
				w.logger.Info("is IsModify..")
				job, err := w.GetJobContentFromKv(event.Kv.Key, event.Kv.Value)
				if err != nil {
					continue
				}

				job.runOn = w.ID
				w.modJob(job)
			case event.Type == clientv3.EventTypeDelete:
				w.logger.Info("is EventTypeDelete..")
				w.delJob(GetIDFromKey(string(event.Kv.Key)))
			default:
				w.logger.Warnf("unknown event type[%v] from job[%s]", event.Type, string(event.Kv.Key))
			}
		}
	})
}

// 立即执行一次任务
func (w *worker) watchOnce() {
	ctx, cancelFunc := NewEtcdTimeoutContext(w)
	defer cancelFunc()

	watch, err := etcd.WatchPrefix(w.Client, ctx, OnceKeyPrefix+w.HostName)
	if err != nil {
		panic(err)
	}

	xgo.Go(func() {
		for event := range watch.C() {
			switch {
			case event.IsCreate(), event.IsModify():
				w.logger.Info("once task...")

				job, err := w.GetOnceJobFromKv(event.Kv.Key, event.Kv.Value)
				if err != nil {
					xlog.Error("get job from kv failed", xlog.String("err", err.Error()))
					continue
				}

				job.worker = w
				go job.RunWithRecovery(WithTaskID(job.TaskID))
			}
		}
	})
}

// watch任务执行列表，执行强杀操作
func (w *worker) watchExecutingProc() {
	ctx, cancelFunc := NewEtcdTimeoutContext(w)
	defer cancelFunc()

	watch, err := etcd.WatchPrefix(w.Client, ctx, ProcKeyPrefix)
	if err != nil {
		panic(err)
	}

	xgo.Go(func() {
		for event := range watch.C() {
			switch {
			case event.IsModify():
				w.logger.Info("exec process task...")

				key := string(event.Kv.Key)
				process, err := GetProcFromKey(key)
				if err != nil {
					w.logger.Warnf("err: %s, kv: %s", err.Error(), event.Kv.String())
					continue
				}

				if process.NodeID != w.ID {
					continue
				}

				val := string(event.Kv.Value)
				pv := &ProcessVal{}
				err = json.Unmarshal([]byte(val), pv)
				if err != nil {
					continue
				}
				process.ProcessVal = *pv
				if process.Killed {
					w.KillExecutingProc(process)
				}
			}
		}
	})
}

func (w *worker) delJob(id string) {
	job, ok := w.jobs[id]
	// 之前此任务没有在当前结点执行
	if !ok {
		return
	}

	delete(w.jobs, id)

	cmds := job.Cmds()
	if len(cmds) == 0 {
		return
	}

	for _, cmd := range cmds {
		w.delCmd(cmd)
	}
	return
}

func (w *worker) modJob(job *Job) {
	oJob, ok := w.jobs[job.ID]
	// 之前此任务没有在当前结点执行，直接增加任务
	if !ok {
		w.addJob(job)
		return
	}

	job.worker = w
	prevCmds := oJob.Cmds()
	*oJob = *job
	cmds := oJob.Cmds()

	// 筛选出需要删除的任务
	for id, cmd := range cmds {
		if util.InStringArray(cmd.Nodes, w.HostName) < 0 {
			continue
		}

		w.modCmd(cmd)
		delete(prevCmds, id)
	}

	for _, cmd := range prevCmds {
		w.delCmd(cmd)
	}
}

func (w *worker) addJob(job *Job) {
	// 添加任务到当前节点
	job.worker = w
	w.jobs[job.ID] = job

	cmds := job.Cmds()
	if len(cmds) == 0 {
		return
	}

	for _, cmd := range cmds {
		if util.InStringArray(cmd.Nodes, w.HostName) < 0 {
			continue
		}

		w.addCmd(cmd)
	}
	return
}

func (w *worker) delCmd(cmd *Cmd) {
	c, ok := w.cmds[cmd.GetID()]
	if ok {
		delete(w.cmds, cmd.GetID())
		w.Cron.Remove(c.schEntryID)
	}
	w.logger.Infof("job[%s] rule[%s] timer[%s] has deleted", cmd.Job.ID, cmd.Timer.ID, cmd.Timer.Cron)
}

func (w *worker) modCmd(cmd *Cmd) {
	c, ok := w.cmds[cmd.GetID()]
	if !ok {
		w.addCmd(cmd)
		return
	}

	entryID := c.schEntryID
	sch := c.Timer.Cron
	*c = *cmd
	c.schEntryID = entryID

	// 节点执行时间改变，更新 cron
	// 否则不用更新 cron
	if c.Timer.Cron != sch {
		w.Cron.Remove(entryID)
		c.schEntryID = w.Cron.Schedule(c.Timer.Schedule, c)
	}

	w.logger.Infof("job[%s]rule[%s] timer[%s] has updated", c.Job.ID, c.Timer.ID, c.Timer.Cron)
}

func (w *worker) addCmd(cmd *Cmd) {
	cmd.schEntryID = w.Cron.Schedule(cmd.Timer.Schedule, cmd)
	w.cmds[cmd.GetID()] = cmd

	w.logger.Infof("job[%s] rule[%s] timer[%s] has added",
		cmd.Job.ID, cmd.Timer.ID, cmd.Timer.Cron)
	return
}

func (w *worker) GetJobContentFromKv(key []byte, value []byte) (*Job, error) {
	job := &Job{}

	if err := json.Unmarshal(value, job); err != nil {
		w.logger.Warnf("job[%s] unmarshal err: %s", key, err.Error())
		return nil, err
	}
	if err := job.ValidRules(); err != nil {
		w.logger.Warnf("valid rules [%s] err: %s", key, err.Error())
		return nil, err
	}

	return job, nil
}

func (w *worker) GetOnceJobFromKv(key []byte, value []byte) (*OnceJob, error) {
	job := &OnceJob{}

	if err := json.Unmarshal(value, job); err != nil {
		w.logger.Warnf("job[%s] unmarshal err: %s", key, err.Error())
		return nil, err
	}
	if err := job.ValidRules(); err != nil {
		w.logger.Warnf("valid rules [%s] err: %s", key, err.Error())
		return nil, err
	}

	return job, nil
}

func (w *worker) KillExecutingProc(process *Process) {
	pid, _ := strconv.Atoi(process.ID)
	if err := killProcess(pid); err != nil {
		w.logger.Warnf("process:[%d] force kill failed, error:[%s]", pid, err)
		return
	}
}
