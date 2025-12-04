package scrapers

import (
	"log"
	"runtime"
	"shared/config"
	"sort"
	"sync"
	"time"
)

type Job interface {
	Run() error
	DisplayName() string
	RunInterval() time.Duration
}

// Can be used to setup a job before running it
type SetupJob interface {
	Job
	Setup() error
}

func MaybeWrapSetupJob(job Job) Job {
	if _, wrapped := job.(*SetupJobWrapper); wrapped {
		return job
	}
	if setupJob, ok := job.(SetupJob); ok {
		return &SetupJobWrapper{SetupJob: setupJob}
	}
	return job
}

type SetupJobWrapper struct {
	SetupJob
	setupRanSuccessfully bool
}

func (s *SetupJobWrapper) Setup() error {
	err := s.SetupJob.Setup()
	if err == nil {
		s.setupRanSuccessfully = true
	}
	return err
}

func (s *SetupJobWrapper) Run() error {
	if !s.setupRanSuccessfully {
		if err := s.Setup(); err != nil {
			return err
		}
	}

	return s.SetupJob.Run()
}

type IntervalHelper struct {
	Interval time.Duration
}

func (i *IntervalHelper) RunInterval() time.Duration {
	return i.Interval
}

func Run(cfg *config.Config, jobs []Job) {
	if len(jobs) == 0 {
		log.Println("No jobs to run")
		return
	}

	log.Println("Started job scheduler")
	maxParallelJobs := cfg.ParallelJobs
	if maxParallelJobs <= 0 {
		maxParallelJobs = runtime.NumCPU() / 2
	}

	type schedItem struct {
		job    Job
		nextAt time.Time
	}

	var queueLock sync.Mutex
	pq := make([]schedItem, len(jobs))
	for i, j := range jobs {
		pq[i] = schedItem{job: j, nextAt: time.Now()}
	}

	// Prevent problems if we have less jobs than maxParallelJobs
	max := maxParallelJobs
	if len(pq) < max {
		max = len(pq)
	}
	sem := make(chan struct{}, max)

	for {
		queueLock.Lock()
		sort.Slice(pq, func(i, j int) bool {
			return pq[i].nextAt.Before(pq[j].nextAt)
		})

		if len(pq) == 0 {
			queueLock.Unlock()

			<-sem
			go func() {
				sem <- struct{}{}
			}()

			continue
		}

		item := pq[0]
		pq = pq[1:]
		queueLock.Unlock()

		until := time.Until(item.nextAt)
		if until > 0 {
			time.Sleep(until)
		}

		sem <- struct{}{}
		go func(si schedItem) {
			defer func() {
				interval := si.job.RunInterval()
				if interval <= 0 {
					interval = cfg.DefaultJobInterval
				}
				si.nextAt = time.Now().Add(interval)

				// Put the job back in the slice
				queueLock.Lock()
				pq = append(pq, si)
				queueLock.Unlock()

				runtime.GC()
				<-sem
			}()

			start := time.Now()
			log.Printf("Running job %s", si.job.DisplayName())
			if err := si.job.Run(); err != nil {
				log.Printf("Job %s failed after %s: %v", si.job.DisplayName(), time.Since(start), err)
			} else {
				log.Printf("Finished job %s in %s", si.job.DisplayName(), time.Since(start))
			}
		}(item)
	}
}
