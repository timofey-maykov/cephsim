# Калибровка и mClock

Без калибровки время recovery считается по значениям по умолчанию: слот hdd 40, ssd 150, nvme 300 MB/s, без предела по объектам и без клиентской нагрузки. Для пулов с мелкими объектами (RGW) это занижает время в разы. Например, на реальном восстановлении RGW-пула модель только по байтам дала 6,6 ч против фактических 17,7 ч.

`cephsim calibrate` достаёт из истории метрик три вещи:

| Что | Куда в модели |
|-----|---------------|
| скорость одного backfill-слота, MB/s, по классам | предел по байтам |
| объектов/с на слот, по классам | предел по объектам |
| клиентская нагрузка на OSD (оп/с, байт/с) по классам | предел mClock |

## Источники

Подойдёт любой Prometheus-совместимый API (Prometheus, VictoriaMetrics: `http://victoria-metrics:8428/select/0/prometheus`); если кластеров несколько, нужный выбирается через `-selector 'cluster="PROD"'`.

| Метрика | Экспортёр | Для чего |
|---------|-----------|----------|
| `ceph_osd_recovery_bytes` | ceph-exporter (порт 9926) или mgr с perf counters | скорость слота, метод osd |
| `ceph_osd_metadata` | mgr | класс OSD (`device_class`) |
| `ceph_pool_recovering_bytes_per_sec` | mgr | скорость слота, метод pool |
| `ceph_pool_recovering_objects_per_sec` | mgr | предел объектов |
| `ceph_pg_backfilling`, `ceph_pg_recovering` | mgr | сколько PG пула в recovery |
| `ceph_osd_op_r`, `_rw`, `_w`, `ceph_osd_op_r_out_bytes`, `ceph_osd_op_w_in_bytes` | ceph-exporter | клиентская нагрузка |

## Скорость слота

**Метод osd** - по умолчанию, если есть per-OSD счётчик:

```
rate(ceph_osd_recovery_bytes[step]) * on(ceph_daemon) group_left(device_class) ceph_osd_metadata
```

1. Замеры OSD, где скорость выше `-min-mbps` (1 MB/s), считаются «OSD занят recovery».
2. По каждому классу берётся перцентиль `-percentile` (p75) этих замеров.
3. Результат делится на `-backfills`, то есть на `osd_max_backfills` того периода.

**Метод pool** - запасной, если per-OSD счётчика нет (например, если ceph-exporter не собирается):

```
скорость PG = ceph_pool_recovering_bytes_per_sec / (ceph_pg_backfilling + ceph_pg_recovering)
```

Класс пула берётся по его CRUSH-правилу из снимка. Дальше - тот же перцентиль по замерам выше порога.

**Предел объектов** считается только по пулам (per-OSD счётчика объектов нет): `ceph_pool_recovering_objects_per_sec` / PG в recovery, перцентиль по классу.

## Клиентская нагрузка

Средняя по OSD класса, по шагам истории:

```
оп/с  = rate(op_r) + rate(op_rw) + rate(op_w) × средний size пулов
байт/с = rate(op_r_out_bytes) + rate(op_w_in_bytes) × средний size пулов
```

Запись умножается на средний `size`, потому что ceph-exporter не отдаёт subop-счётчики: каждая клиентская запись ложится на `size` OSD, а счётчик есть только у primary.

Сохраняются p50, p95, p99 и среднее в моменты, когда шёл recovery (`recovery`). `simulate -client-load` выбирает одно из них, по умолчанию p95.

## mClock

С `osd_op_queue = mclock_scheduler` OSD делит ёмкость между клиентами и фоновым recovery. Модель повторяет расчёт из `ceph/src/common/mclock_common.h`.

| Параметр | Откуда |
|----------|--------|
| ёмкость, байт/с | `osd_mclock_max_sequential_bandwidth_{hdd,ssd}` |
| цена операции | `bandwidth / osd_mclock_max_capacity_iops_{hdd,ssd}` (HDD по умолчанию: 150 MiB/s / 315 ≈ 500 KB) |
| резервы, веса, лимиты | профиль: `high_client_ops`, `high_recovery_ops`, `balanced` или `custom` |

| Профиль | клиенты: резерв / вес | recovery: резерв / вес |
|---------|-----------------------|------------------------|
| `high_client_ops` | 60% / 2 | 40% / 1 |
| `balanced` | 50% / 1 | 50% / 1 |
| `high_recovery_ops` | 30% / 1 | 70% / 2 |

Пока recovery идёт, клиентам достаётся не больше своего резерва плюс доли свободной части по весам. Recovery забирает всё, что клиенты не используют: планировщик работает с запасом. Каждый объект recovery стоит одну операцию плюс свои байты, поэтому на HDD миллион мелких объектов - это около 500 GB «стоимости» сверх самих данных.

**Когда mClock не моделируется:**
- `osd_op_queue = wpq`;
- нет `osd_mclock_max_sequential_bandwidth_*`: так в Quincy 17.2.6 и раньше, там другая модель цены операций (`osd_mclock_cost_per_io_usec`).;
- в `meta.json` нет блока `mclock` (снимок без collect).

В этих случаях время считается только по пределам байт и объектов из калибровки.

**`osd_max_backfills` при mClock.** Начиная с Quincy, при mClock `osd_max_backfills` не действует, пока не включён `osd_mclock_override_recovery_settings`: число параллельных backfill задаёт профиль. Поэтому:
- калибруйте на том же профиле, на котором будут работы;
- в `calibrate -backfills` ставьте значение, действовавшее в период истории (`ceph config show osd.N osd_max_backfills`);
- в `simulate -backfills` понимайте как множитель к откалиброванной скорости, а не как гарантию.

## Типичные проблемы

| Симптом | Причина | Что делать |
|---------|---------|------------|
| `нет рядов ceph_osd_recovery_bytes` | нет ceph-exporter или не собирается | метод pool подхватится сам (`-source auto`) |
| `клиентская нагрузка не снята` | нет `ceph_osd_op_*` | ceph-exporter не собирается или его job пропал из конфига сборщика; вернуть сбор, потом калибровать |
| у класса единицы замеров | recovery на этом классе почти не было | задать `-slot-mbps` и `-slot-ops` вручную по аналогичному кластеру |
| `слот MB/s` у HDD = 40 ровно | класс не откалиброван, стоит значение по умолчанию | в истории не было recovery на этом классе |
| время recovery в разы меньше факта | нет предела по объектам, пул с мелкими объектами | откалибровать, или `-slot-ops`; отчёт предупредит |
