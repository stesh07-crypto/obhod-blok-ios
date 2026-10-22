package main

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

type schedulerTestClock struct {
	ns atomic.Int64
}

func newSchedulerTestClock() *schedulerTestClock {
	c := &schedulerTestClock{}
	// Держим стартовую точку рядом с реальными часами, чтобы time.Until для
	// ожидающих Acquire не уходил в прошлое и не крутил цикл вхолостую.
	c.ns.Store(time.Now().UnixNano())
	return c
}

func (c *schedulerTestClock) Now() time.Time { return time.Unix(0, c.ns.Load()) }

func (c *schedulerTestClock) Advance(d time.Duration) { c.ns.Add(int64(d)) }

func schedulerTestCandidates(t *testing.T, urls ...string) []turnEndpointCandidate {
	t.Helper()
	candidates, err := buildTurnEndpointCandidates(urls, &TurnParams{})
	if err != nil {
		t.Fatalf("buildTurnEndpointCandidates: %v", err)
	}
	return candidates
}

func mustAcquireTurnPermit(t *testing.T, s *turnScheduler, preferred string, candidates []turnEndpointCandidate) *turnPermit {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	permit, err := s.Acquire(ctx, preferred, candidates)
	if err != nil {
		t.Fatalf("Acquire(%s): %v", preferred, err)
	}
	return permit
}

func findEndpointSnapshot(t *testing.T, s *turnScheduler, endpoint string) turnEndpointSnapshot {
	t.Helper()
	for _, snapshot := range s.Snapshot() {
		if snapshot.Endpoint == endpoint {
			return snapshot
		}
	}
	t.Fatalf("endpoint %s not found in snapshot %+v", endpoint, s.Snapshot())
	return turnEndpointSnapshot{}
}

func TestTurnSchedulerSecondEndpointWorksWhileFirstHangs(t *testing.T) {
	clock := newSchedulerTestClock()
	s := newTurnScheduler(make(chan struct{}, 3), clock.Now)
	candidates := schedulerTestCandidates(t,
		"turn:1.1.1.1:3478",
		"turn:2.2.2.2:3478",
	)

	// Первый endpoint завис в handshake и удерживает своё разрешение.
	hung := mustAcquireTurnPermit(t, s, "turn:1.1.1.1:3478", candidates)
	if hung.Endpoint() != "1.1.1.1:3478" {
		t.Fatalf("первый endpoint = %s", hung.Endpoint())
	}

	// Второй воркер с тем же предпочтением должен уйти на свободный endpoint,
	// не дожидаясь таймаута первого.
	second := mustAcquireTurnPermit(t, s, "turn:1.1.1.1:3478", candidates)
	if second.Endpoint() != "2.2.2.2:3478" {
		t.Fatalf("второй endpoint = %s, ожидался 2.2.2.2:3478", second.Endpoint())
	}

	// Третья попытка при занятых A и B ждёт endpoint, но НЕ занимает
	// свободный третий глобальный слот.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result := make(chan *turnPermit, 1)
	errCh := make(chan error, 1)
	go func() {
		permit, err := s.Acquire(ctx, "turn:1.1.1.1:3478", candidates)
		if err != nil {
			errCh <- err
			return
		}
		result <- permit
	}()

	time.Sleep(100 * time.Millisecond)
	if got := len(s.global); got != 2 {
		t.Fatalf("ожидающая endpoint попытка заняла глобальный слот: занято %d, ожидалось 2", got)
	}
	select {
	case permit := <-result:
		permit.Finish(turnOutcomeOther)
		t.Fatal("третья попытка не должна начинаться при занятых A и B")
	case err := <-errCh:
		t.Fatalf("третья попытка завершилась ошибкой: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	second.Finish(turnOutcomeSuccess)
	var third *turnPermit
	select {
	case third = <-result:
		if third.Endpoint() != "2.2.2.2:3478" {
			t.Fatalf("третий endpoint = %s, ожидался 2.2.2.2:3478", third.Endpoint())
		}
	case err := <-errCh:
		t.Fatalf("третья попытка завершилась ошибкой: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("третья попытка не дождалась освобождения endpoint")
	}
	third.Finish(turnOutcomeSuccess)

	// Первый завершился таймаутом. После одного таймаута паузы ещё нет:
	// следующая попытка снова может выбрать 1.1.1.1.
	hung.Finish(turnOutcomeTimeout)
	retry := mustAcquireTurnPermit(t, s, "turn:1.1.1.1:3478", candidates)
	if retry.Endpoint() != "1.1.1.1:3478" {
		t.Fatalf("retry endpoint = %s, ожидался 1.1.1.1:3478", retry.Endpoint())
	}
	retry.Finish(turnOutcomeSuccess)
}

func TestTurnSchedulerGlobalLimitStaysThree(t *testing.T) {
	clock := newSchedulerTestClock()
	s := newTurnScheduler(make(chan struct{}, 3), clock.Now)
	candidates := schedulerTestCandidates(t,
		"turn:1.1.1.1:3478",
		"turn:2.2.2.2:3478",
		"turn:3.3.3.3:3478",
		"turn:4.4.4.4:3478",
	)

	var permits []*turnPermit
	for _, url := range []string{
		"turn:1.1.1.1:3478",
		"turn:2.2.2.2:3478",
		"turn:3.3.3.3:3478",
	} {
		permits = append(permits, mustAcquireTurnPermit(t, s, url, candidates))
	}
	if got := len(s.global); got != 3 {
		t.Fatalf("занято глобальных слотов = %d, ожидалось 3", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result := make(chan *turnPermit, 1)
	errCh := make(chan error, 1)
	go func() {
		permit, err := s.Acquire(ctx, "turn:4.4.4.4:3478", candidates)
		if err != nil {
			errCh <- err
			return
		}
		result <- permit
	}()

	select {
	case permit := <-result:
		permit.Finish(turnOutcomeOther)
		t.Fatal("четвёртый поток получил глобальный слот при занятых трёх")
	case err := <-errCh:
		t.Fatalf("четвёртая попытка завершилась ошибкой: %v", err)
	case <-time.After(150 * time.Millisecond):
	}

	permits[0].Finish(turnOutcomeSuccess)

	select {
	case permit := <-result:
		if permit.Endpoint() != "4.4.4.4:3478" {
			t.Fatalf("четвёртый endpoint = %s", permit.Endpoint())
		}
		permit.Finish(turnOutcomeSuccess)
	case err := <-errCh:
		t.Fatalf("четвёртая попытка завершилась ошибкой: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("четвёртая попытка не получила слот после освобождения")
	}

	permits[1].Finish(turnOutcomeSuccess)
	permits[2].Finish(turnOutcomeSuccess)
	if got := len(s.global); got != 0 {
		t.Fatalf("после release занято глобальных слотов = %d", got)
	}
}

func TestTurnSchedulerRetryMovesToNextUniqueEndpoint(t *testing.T) {
	clock := newSchedulerTestClock()
	s := newTurnScheduler(make(chan struct{}, 3), clock.Now)
	urls := []string{
		"turn:1.1.1.1:3478",
		"turn:2.2.2.2:3478",
	}
	candidates := schedulerTestCandidates(t, urls...)

	first := mustAcquireTurnPermit(t, s, urls[0], candidates)
	if first.Endpoint() != "1.1.1.1:3478" {
		t.Fatalf("первая попытка выбрала %s", first.Endpoint())
	}
	first.Finish(turnOutcomeTimeout)

	nextURL := nextUniqueTurnURL(urls, &TurnParams{}, first.Endpoint())
	if nextURL != urls[1] {
		t.Fatalf("после таймаута next=%q, ожидался %q", nextURL, urls[1])
	}
	retry := mustAcquireTurnPermit(t, s, nextURL, candidates)
	if retry.Endpoint() != "2.2.2.2:3478" {
		t.Fatalf("retry выбрал %s, ожидался 2.2.2.2:3478", retry.Endpoint())
	}
	retry.Finish(turnOutcomeSuccess)
}

func TestTurnSchedulerPausesAfterTwoTimeoutsAndAllowsProbe(t *testing.T) {
	clock := newSchedulerTestClock()
	s := newTurnScheduler(make(chan struct{}, 3), clock.Now)
	candidates := schedulerTestCandidates(t,
		"turn:1.1.1.1:3478",
		"turn:2.2.2.2:3478",
	)

	first := mustAcquireTurnPermit(t, s, "turn:1.1.1.1:3478", candidates)
	first.Finish(turnOutcomeTimeout)
	second := mustAcquireTurnPermit(t, s, "turn:1.1.1.1:3478", candidates)
	if second.Endpoint() != "1.1.1.1:3478" {
		t.Fatalf("второй таймаут должен быть на 1.1.1.1, получен %s", second.Endpoint())
	}
	second.Finish(turnOutcomeTimeout)

	snapshot := findEndpointSnapshot(t, s, "1.1.1.1:3478")
	if snapshot.PausedUntil.IsZero() {
		t.Fatal("после двух DTLS_TIMEOUT endpoint не поставлен на паузу")
	}

	// На паузе предпочтительный плохой endpoint заменяется альтернативой.
	alternative := mustAcquireTurnPermit(t, s, "turn:1.1.1.1:3478", candidates)
	if alternative.Endpoint() != "2.2.2.2:3478" {
		t.Fatalf("на паузе выбран %s, ожидался 2.2.2.2:3478", alternative.Endpoint())
	}
	alternative.Finish(turnOutcomeSuccess)

	// Через 60 секунд разрешается одна пробная попытка.
	clock.Advance(60 * time.Second)
	probe := mustAcquireTurnPermit(t, s, "turn:1.1.1.1:3478", candidates)
	if probe.Endpoint() != "1.1.1.1:3478" {
		t.Fatalf("после паузы выбрана не пробная попытка: %s", probe.Endpoint())
	}
	probe.Finish(turnOutcomeTimeout)

	snapshot = findEndpointSnapshot(t, s, "1.1.1.1:3478")
	if snapshot.PausedUntil.IsZero() {
		t.Fatal("неудачная пробная попытка должна снова поставить endpoint на паузу")
	}

	// Ещё через 60 секунд успешная проба возвращает endpoint в обычную работу.
	clock.Advance(60 * time.Second)
	probe = mustAcquireTurnPermit(t, s, "turn:1.1.1.1:3478", candidates)
	if probe.Endpoint() != "1.1.1.1:3478" {
		t.Fatalf("вторая проба выбрала %s", probe.Endpoint())
	}
	probe.Finish(turnOutcomeSuccess)
	normal := mustAcquireTurnPermit(t, s, "turn:1.1.1.1:3478", candidates)
	if normal.Endpoint() != "1.1.1.1:3478" {
		t.Fatalf("после успешной пробы endpoint не восстановлен: %s", normal.Endpoint())
	}
	normal.Finish(turnOutcomeSuccess)
}

func TestTurnPermitFinishIsIdempotent(t *testing.T) {
	clock := newSchedulerTestClock()
	s := newTurnScheduler(make(chan struct{}, 3), clock.Now)
	candidates := schedulerTestCandidates(t, "turn:1.1.1.1:3478")
	permit := mustAcquireTurnPermit(t, s, "turn:1.1.1.1:3478", candidates)
	permit.Finish(turnOutcomeSuccess)
	permit.Finish(turnOutcomeTimeout)
	permit.Finish(turnOutcomeCanceled)
	if got := len(s.global); got != 0 {
		t.Fatalf("после повторного Finish занято %d слотов", got)
	}
	if snapshot := findEndpointSnapshot(t, s, "1.1.1.1:3478"); snapshot.Active != 0 {
		t.Fatalf("после повторного Finish active=%d", snapshot.Active)
	}
}

func TestTurnSchedulerResetClearsPause(t *testing.T) {
	clock := newSchedulerTestClock()
	s := newTurnScheduler(make(chan struct{}, 3), clock.Now)
	candidates := schedulerTestCandidates(t,
		"turn:1.1.1.1:3478",
		"turn:2.2.2.2:3478",
	)

	for i := 0; i < turnEndpointFailureLimit; i++ {
		permit := mustAcquireTurnPermit(t, s, "turn:1.1.1.1:3478", candidates)
		permit.Finish(turnOutcomeTimeout)
	}
	if snapshot := findEndpointSnapshot(t, s, "1.1.1.1:3478"); snapshot.PausedUntil.IsZero() {
		t.Fatal("endpoint не на паузе перед Reset")
	}

	s.Reset()
	if got := len(s.Snapshot()); got != 0 {
		t.Fatalf("после Reset осталось %d endpoint-записей", got)
	}
	permit := mustAcquireTurnPermit(t, s, "turn:1.1.1.1:3478", candidates)
	if permit.Endpoint() != "1.1.1.1:3478" {
		t.Fatalf("после Reset выбран %s", permit.Endpoint())
	}
	permit.Finish(turnOutcomeSuccess)
}

func TestTurnSchedulerWaitsForPausedEndpointWithoutGlobalSlot(t *testing.T) {
	clock := newSchedulerTestClock()
	s := newTurnScheduler(make(chan struct{}, 3), clock.Now)
	candidates := schedulerTestCandidates(t,
		"turn:1.1.1.1:3478",
		"turn:2.2.2.2:3478",
	)

	for i := 0; i < turnEndpointFailureLimit; i++ {
		permit := mustAcquireTurnPermit(t, s, "turn:1.1.1.1:3478", candidates)
		permit.Finish(turnOutcomeTimeout)
	}

	// B занят, A на паузе: новый Acquire должен ждать паузу, не занимая
	// глобальный слот.
	b := mustAcquireTurnPermit(t, s, "turn:2.2.2.2:3478", candidates)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result := make(chan *turnPermit, 1)
	errCh := make(chan error, 1)
	go func() {
		permit, err := s.Acquire(ctx, "turn:1.1.1.1:3478", candidates)
		if err != nil {
			errCh <- err
			return
		}
		result <- permit
	}()

	time.Sleep(100 * time.Millisecond)
	if got := len(s.global); got != 1 {
		t.Fatalf("ожидающая паузу попытка заняла глобальный слот: занято %d, ожидалось 1", got)
	}
	select {
	case permit := <-result:
		permit.Finish(turnOutcomeOther)
		t.Fatal("попытка не должна выбирать A до истечения паузы")
	case <-errCh:
		t.Fatal("Acquire завершился ошибкой до истечения паузы")
	case <-time.After(50 * time.Millisecond):
	}

	clock.Advance(turnEndpointPause)
	s.broadcast()

	select {
	case permit := <-result:
		if permit.Endpoint() != "1.1.1.1:3478" {
			t.Fatalf("после паузы выбрана не пробная попытка: %s", permit.Endpoint())
		}
		permit.Finish(turnOutcomeSuccess)
	case err := <-errCh:
		t.Fatalf("Acquire завершился ошибкой после паузы: %v", err)
	case <-time.After(time.Second):
		t.Fatal("Acquire не проснулся после истечения паузы")
	}
	b.Finish(turnOutcomeSuccess)
}

func TestTurnSchedulerResetPreservesActivePermits(t *testing.T) {
	clock := newSchedulerTestClock()
	s := newTurnScheduler(make(chan struct{}, 3), clock.Now)
	candidates := schedulerTestCandidates(t,
		"turn:1.1.1.1:3478",
		"turn:2.2.2.2:3478",
	)
	a := mustAcquireTurnPermit(t, s, "turn:1.1.1.1:3478", candidates)
	b := mustAcquireTurnPermit(t, s, "turn:2.2.2.2:3478", candidates)

	s.Reset()
	if snapshot := findEndpointSnapshot(t, s, "1.1.1.1:3478"); snapshot.Active != 1 {
		t.Fatalf("Reset потерял активное разрешение A: active=%d", snapshot.Active)
	}
	if snapshot := findEndpointSnapshot(t, s, "2.2.2.2:3478"); snapshot.Active != 1 {
		t.Fatalf("Reset потерял активное разрешение B: active=%d", snapshot.Active)
	}
	if got := len(s.global); got != 2 {
		t.Fatalf("Reset не должен освобождать глобальные слоты: занято %d", got)
	}

	a.Finish(turnOutcomeSuccess)
	b.Finish(turnOutcomeSuccess)
	for _, snapshot := range s.Snapshot() {
		if snapshot.Active != 0 {
			t.Fatalf("после Finish active=%d у %s", snapshot.Active, snapshot.Endpoint)
		}
	}
}

func TestTurnSchedulerSingleEndpointAllowsConcurrency(t *testing.T) {
	clock := newSchedulerTestClock()
	s := newTurnScheduler(make(chan struct{}, 3), clock.Now)
	candidates := schedulerTestCandidates(t, "turn:1.1.1.1:3478")

	// Для единственного endpoint ограничение "один handshake" отключено,
	// иначе группа не смогла бы запуститься.
	p1 := mustAcquireTurnPermit(t, s, "turn:1.1.1.1:3478", candidates)
	p2 := mustAcquireTurnPermit(t, s, "turn:1.1.1.1:3478", candidates)
	p3 := mustAcquireTurnPermit(t, s, "turn:1.1.1.1:3478", candidates)
	if got := len(s.global); got != 3 {
		t.Fatalf("для одного endpoint занято %d слотов, ожидалось 3", got)
	}

	p1.Finish(turnOutcomeTimeout)
	p2.Finish(turnOutcomeTimeout)
	p3.Finish(turnOutcomeTimeout)

	snapshot := findEndpointSnapshot(t, s, "1.1.1.1:3478")
	if !snapshot.PausedUntil.IsZero() {
		t.Fatal("единственный endpoint не должен блокироваться паузой")
	}
	retry := mustAcquireTurnPermit(t, s, "turn:1.1.1.1:3478", candidates)
	retry.Finish(turnOutcomeSuccess)
}

func TestTurnSchedulerCanceledWaitDoesNotLeakReservation(t *testing.T) {
	clock := newSchedulerTestClock()
	s := newTurnScheduler(make(chan struct{}, 3), clock.Now)
	candidates := schedulerTestCandidates(t,
		"turn:1.1.1.1:3478",
		"turn:2.2.2.2:3478",
	)

	a := mustAcquireTurnPermit(t, s, "turn:1.1.1.1:3478", candidates)
	b := mustAcquireTurnPermit(t, s, "turn:2.2.2.2:3478", candidates)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := s.Acquire(ctx, "turn:1.1.1.1:3478", candidates)
		errCh <- err
	}()

	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ожидался context.Canceled, получено %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Acquire не завершился после отмены")
	}

	a.Finish(turnOutcomeCanceled)
	b.Finish(turnOutcomeCanceled)
	for _, snapshot := range s.Snapshot() {
		if snapshot.Active != 0 {
			t.Fatalf("после отмены/освобождения active=%d у %s", snapshot.Active, snapshot.Endpoint)
		}
	}
}

func TestTurnSchedulerCanceledGlobalWaitReleasesEndpoint(t *testing.T) {
	clock := newSchedulerTestClock()
	s := newTurnScheduler(make(chan struct{}, 3), clock.Now)
	candidates := schedulerTestCandidates(t,
		"turn:1.1.1.1:3478",
		"turn:2.2.2.2:3478",
		"turn:3.3.3.3:3478",
		"turn:4.4.4.4:3478",
	)
	p1 := mustAcquireTurnPermit(t, s, "turn:1.1.1.1:3478", candidates)
	p2 := mustAcquireTurnPermit(t, s, "turn:2.2.2.2:3478", candidates)
	p3 := mustAcquireTurnPermit(t, s, "turn:3.3.3.3:3478", candidates)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		// 4.4.4.4 резервируется, затем ждёт глобальный слот.
		_, err := s.Acquire(ctx, "turn:4.4.4.4:3478", candidates)
		errCh <- err
	}()

	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ожидался context.Canceled, получено %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Acquire не завершился после отмены")
	}

	snapshot := findEndpointSnapshot(t, s, "4.4.4.4:3478")
	if snapshot.Active != 0 {
		t.Fatalf("после отмены у 4.4.4.4 active=%d", snapshot.Active)
	}

	p1.Finish(turnOutcomeSuccess)
	p2.Finish(turnOutcomeSuccess)
	p3.Finish(turnOutcomeSuccess)
}

func TestNextUniqueTurnURL(t *testing.T) {
	urls := []string{
		"turn:1.1.1.1:3478",
		"turn:2.2.2.2:3478",
	}
	if got := nextUniqueTurnURL(urls, &TurnParams{}, "1.1.1.1:3478"); got != urls[1] {
		t.Fatalf("next после 1.1.1.1 = %q, ожидался %q", got, urls[1])
	}
	if got := nextUniqueTurnURL(urls, &TurnParams{}, "2.2.2.2:3478"); got != urls[0] {
		t.Fatalf("next после 2.2.2.2 = %q, ожидался %q", got, urls[0])
	}
	if got := nextUniqueTurnURL(urls[:1], &TurnParams{}, "1.1.1.1:3478"); got != "" {
		t.Fatalf("для единственного endpoint получен %q", got)
	}
	if got := nextUniqueTurnURL([]string{urls[0], urls[0]}, &TurnParams{}, "1.1.1.1:3478"); got != "" {
		t.Fatalf("дубликат endpoint не должен считаться альтернативой: %q", got)
	}
}

func TestBuildTurnEndpointCandidatesDeduplicatesEndpoints(t *testing.T) {
	candidates, err := buildTurnEndpointCandidates([]string{
		"turn:1.1.1.1:3478?transport=udp",
		"turn:1.1.1.1:3478?transport=tcp",
		"turn:2.2.2.2:3478",
	}, &TurnParams{})
	if err != nil {
		t.Fatalf("buildTurnEndpointCandidates: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("получено %d endpoint, ожидалось 2: %+v", len(candidates), candidates)
	}
	if candidates[0].Endpoint != "1.1.1.1:3478" || candidates[1].Endpoint != "2.2.2.2:3478" {
		t.Fatalf("неверные endpoint: %+v", candidates)
	}

	overridden, err := buildTurnEndpointCandidates([]string{
		"turn:1.1.1.1:3478",
		"turn:2.2.2.2:3478",
	}, &TurnParams{Host: "9.9.9.9", Port: "5000"})
	if err != nil {
		t.Fatalf("buildTurnEndpointCandidates override: %v", err)
	}
	if len(overridden) != 1 || overridden[0].Endpoint != "9.9.9.9:5000" {
		t.Fatalf("override не схлопнул endpoint: %+v", overridden)
	}
}

func TestDispatcherKeepsDataFlowingWhileEndpointDegrades(t *testing.T) {
	stats := NewStats()
	localConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	dispatcher := NewDispatcher(ctx, localConn, stats)
	defer func() {
		cancel()
		_ = localConn.Close()
		dispatcher.Shutdown()
	}()

	slot := &WorkerSlot{ID: 1, SendCh: make(chan []byte, 4)}
	dispatcher.Register(slot)
	defer dispatcher.Unregister(slot)

	client, err := net.DialUDP("udp", nil, localConn.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatalf("DialUDP: %v", err)
	}
	defer client.Close()

	// Устанавливаем clientAddr в Dispatcher и проверяем путь до worker.
	if _, err := client.Write([]byte("upstream")); err != nil {
		t.Fatalf("client.Write: %v", err)
	}
	select {
	case pkt := <-slot.SendCh:
		if string(pkt) != "upstream" {
			t.Fatalf("worker получил %q", pkt)
		}
		putPktBuf(pkt)
	case <-time.After(time.Second):
		t.Fatal("Dispatcher не передал пакет worker'у")
	}

	// Деградация TURN endpoint не должна затрагивать уже работающий
	// Dispatcher/worker data path.
	clock := newSchedulerTestClock()
	s := newTurnScheduler(make(chan struct{}, 3), clock.Now)
	candidates := schedulerTestCandidates(t,
		"turn:1.1.1.1:3478",
		"turn:2.2.2.2:3478",
	)
	for i := 0; i < turnEndpointFailureLimit; i++ {
		permit := mustAcquireTurnPermit(t, s, "turn:1.1.1.1:3478", candidates)
		permit.Finish(turnOutcomeTimeout)
	}
	permit := mustAcquireTurnPermit(t, s, "turn:1.1.1.1:3478", candidates)
	if permit.Endpoint() != "2.2.2.2:3478" {
		t.Fatalf("при деградации выбран %s", permit.Endpoint())
	}
	permit.Finish(turnOutcomeSuccess)

	// Обратный путь через Dispatcher всё ещё работает.
	down := getPktBuf(len("downstream"))
	copy(down, "downstream")
	dispatcher.ReturnCh <- down
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 64)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatalf("client.Read: %v", err)
	}
	if string(buf[:n]) != "downstream" {
		t.Fatalf("клиент получил %q", buf[:n])
	}
}

func TestRunSessionCancellationReleasesTurnPermit(t *testing.T) {
	turnEndpointScheduler.Reset()
	t.Cleanup(turnEndpointScheduler.Reset)

	// UDP-сокет, который принимает Allocate и молчит: RunSession останется
	// в сетевой подготовке, пока тест не отменит контекст.
	blackhole, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	defer blackhole.Close()

	turnURL := blackhole.LocalAddr().String()
	host, port, err := net.SplitHostPort(turnURL)
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	tp := &TurnParams{Host: host, Port: port}
	creds := &Credentials{
		User:          "test-user",
		Pass:          "test-pass",
		TurnURLs:      []string{turnURL},
		CacheStreamID: 999,
		Generation:    1,
		GroupID:       1,
	}
	peer := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := RunSession(ctx, tp, peer, nil, "0", nil, nil, 1, creds, "dev", "pass", NewStats())
		done <- err
	}()

	deadline := time.Now().Add(2 * time.Second)
	active := false
	for time.Now().Before(deadline) {
		for _, snapshot := range turnEndpointScheduler.Snapshot() {
			if snapshot.Endpoint == turnURL && snapshot.Active > 0 {
				active = true
				break
			}
		}
		if active {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !active {
		t.Fatal("RunSession не получил разрешение TURN endpoint")
	}

	// Дадим Allocate начать сетевую операцию, затем отменим пользователем.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("RunSession должен вернуть ошибку после отмены")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunSession не завершился после отмены")
	}

	if got := len(handshakeSem); got != 0 {
		t.Fatalf("после отмены занято глобальных слотов: %d", got)
	}
	if got := HandshakesInFlight.Load(); got != 0 {
		t.Fatalf("после отмены HandshakesInFlight=%d", got)
	}
	snapshot := findEndpointSnapshot(t, turnEndpointScheduler, turnURL)
	if snapshot.Active != 0 {
		t.Fatalf("после отмены endpoint active=%d", snapshot.Active)
	}
	if snapshot.Failures != 0 {
		t.Fatalf("отмена пользователя не должна считаться DTLS_TIMEOUT: failures=%d", snapshot.Failures)
	}
}

func TestDTLSHandshakeTimeoutErrorPreservesEndpoint(t *testing.T) {
	base := errors.New("deadline")
	err := &DTLSHandshakeTimeoutError{Endpoint: "1.1.1.1:3478", TurnURL: "turn:1.1.1.1:3478", Err: base}
	if !isTimeoutError(err) {
		t.Fatal("DTLSHandshakeTimeoutError должен распознаваться как timeout")
	}
	if !errors.Is(err, base) {
		t.Fatal("DTLSHandshakeTimeoutError должен разворачиваться до исходной ошибки")
	}
	var target *DTLSHandshakeTimeoutError
	if !errors.As(err, &target) || target.Endpoint != "1.1.1.1:3478" {
		t.Fatalf("errors.As не вернул endpoint: %+v", target)
	}
}

func TestTurnSchedulerResetThenCancelGlobalWaitReleasesEndpoint(t *testing.T) {
	clock := newSchedulerTestClock()
	s := newTurnScheduler(make(chan struct{}, 3), clock.Now)
	candidates := schedulerTestCandidates(t,
		"turn:1.1.1.1:3478",
		"turn:2.2.2.2:3478",
		"turn:3.3.3.3:3478",
		"turn:4.4.4.4:3478",
	)

	// Займи все три глобальных слота разрешениями для адресов A, B и C.
	p1 := mustAcquireTurnPermit(t, s, "turn:1.1.1.1:3478", candidates)
	p2 := mustAcquireTurnPermit(t, s, "turn:2.2.2.2:3478", candidates)
	p3 := mustAcquireTurnPermit(t, s, "turn:3.3.3.3:3478", candidates)
	if got := len(s.global); got != 3 {
		t.Fatalf("занято глобальных слотов = %d, ожидалось 3", got)
	}

	// Запусти Acquire() для D с набором альтернатив.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		_, err := s.Acquire(ctx, "turn:4.4.4.4:3478", candidates)
		errCh <- err
	}()

	// Дождись подтверждения, что D зарезервирован (active == 1), но глобальный слот ещё не получен.
	deadline := time.Now().Add(2 * time.Second)
	for {
		snap := findEndpointSnapshot(t, s, "4.4.4.4:3478")
		if snap.Active == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("endpoint D не перешёл в active == 1 до таймаута")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := len(s.global); got != 3 {
		t.Fatalf("ожидающая D попытка не должна занимать глобальный слот: занято %d", got)
	}

	// Вызови Reset().
	s.Reset()

	// Отмени контекст ожидающего Acquire() для D.
	cancel()

	// Проверь, что он вернул context.Canceled.
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ожидался context.Canceled, получено: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire не завершился после отмены контекста")
	}

	// Проверь через Snapshot() актуальную карту: у D active == 0.
	snapD := findEndpointSnapshot(t, s, "4.4.4.4:3478")
	if snapD.Active != 0 {
		t.Fatalf("после отмены у D active = %d, ожидалось 0", snapD.Active)
	}

	// Проверь, что три исходных глобальных слота всё ещё заняты.
	if got := len(s.global); got != 3 {
		t.Fatalf("исходные глобальные слоты должны быть заняты: занято %d, ожидалось 3", got)
	}

	// Освободи исходные разрешения и проверь, что глобальных слотов и активных резерваций не осталось.
	p1.Finish(turnOutcomeSuccess)
	p2.Finish(turnOutcomeSuccess)
	p3.Finish(turnOutcomeSuccess)

	if got := len(s.global); got != 0 {
		t.Fatalf("после освобождения исходных разрешений занято глобальных слотов: %d, ожидалось 0", got)
	}
	for _, snap := range s.Snapshot() {
		if snap.Active != 0 {
			t.Fatalf("после освобождения у %s active = %d, ожидалось 0", snap.Endpoint, snap.Active)
		}
	}

	// Повторно запроси D и проверь, что разрешение успешно получается и освобождается.
	pD := mustAcquireTurnPermit(t, s, "turn:4.4.4.4:3478", candidates)
	if pD.Endpoint() != "4.4.4.4:3478" {
		t.Fatalf("повторный запрос D выбрал %s, ожидался 4.4.4.4:3478", pD.Endpoint())
	}
	pD.Finish(turnOutcomeSuccess)
	if snap := findEndpointSnapshot(t, s, "4.4.4.4:3478"); snap.Active != 0 {
		t.Fatalf("после Finish pD active = %d, ожидалось 0", snap.Active)
	}
	if got := len(s.global); got != 0 {
		t.Fatalf("после Finish pD занято глобальных слотов: %d, ожидалось 0", got)
	}
}

