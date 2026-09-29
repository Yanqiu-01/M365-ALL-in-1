package web

import (
	"context"
	"sync"
	"testing"
)

func TestFailoverUsesSchedulerInsteadOfStoredOrder(t *testing.T) {
	s := &Server{tokens: testAccountFiles(t), accountPool: newAccountHealth(), resourceScheduler: newResourceScheduler()}
	excluded := map[string]bool{"u-3": true}
	counts := map[string]int{}
	for i := 0; i < 12; i++ {
		account, err := s.nextHealthyAccountExcept(excluded)
		if err != nil {
			t.Fatal(err)
		}
		counts[account.ID]++
	}
	if counts["u-1"] != 6 || counts["u-2"] != 6 || counts["u-3"] != 0 {
		t.Fatalf("failover must distribute eligible candidates, got %v", counts)
	}
	if len(excluded) != 1 || !excluded["u-3"] {
		t.Fatalf("modified caller exclusions: %v", excluded)
	}
}

func TestFailoverSharesNewRequestScheduling(t *testing.T) {
	s := &Server{tokens: testAccountFiles(t), accountPool: newAccountHealth(), resourceScheduler: newResourceScheduler()}
	first, err := s.resolveAccount("")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.nextHealthyAccountExcept(nil)
	if err != nil {
		t.Fatal(err)
	}
	third, err := s.resolveAccount("")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID || first.ID == third.ID || second.ID == third.ID {
		t.Fatalf("new requests and failover must share selection counts: %s, %s, %s", first.ID, second.ID, third.ID)
	}
}

func TestFailoverPrefersLowerInflight(t *testing.T) {
	s := &Server{tokens: testAccountFiles(t), accountPool: newAccountHealth(), accountConcurrency: newAccountConcurrency(), resourceScheduler: newResourceScheduler()}
	release, err := s.accountConcurrency.Acquire(context.Background(), "u-1")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	account, err := s.nextHealthyAccount("u-3")
	if err != nil || account.ID != "u-2" {
		t.Fatalf("selected busy candidate: account=%s err=%v", account.ID, err)
	}
}

func TestFailoverConcurrentRequestsRemainFair(t *testing.T) {
	s := &Server{tokens: testAccountFiles(t), accountPool: newAccountHealth(), resourceScheduler: newResourceScheduler()}
	excluded := map[string]bool{"u-3": true}
	ids := make(chan string, 40)
	var wg sync.WaitGroup
	for i := 0; i < cap(ids); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			account, err := s.nextHealthyAccountExcept(excluded)
			if err != nil {
				t.Error(err)
				return
			}
			ids <- account.ID
		}()
	}
	wg.Wait()
	close(ids)
	counts := map[string]int{}
	for id := range ids {
		counts[id]++
	}
	if counts["u-1"] != 20 || counts["u-2"] != 20 || counts["u-3"] != 0 {
		t.Fatalf("concurrent failover distribution=%v", counts)
	}
}
