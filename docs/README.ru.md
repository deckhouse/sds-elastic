---
title: "Модуль sds-elastic"
description: "Модуль Deckhouse Kubernetes Platform, который разворачивает кластер Rook Ceph поверх BlockDevice CR и публикует пулы через StorageClass'ы csi-ceph."
weight: 1
---

{{< alert level="warning" >}}
Модуль находится в стадии `Experimental`. API, настройки и кастомные ресурсы могут меняться без предупреждения; не используйте его в production-нагрузках.
{{< /alert >}}

Модуль `sds-elastic` устанавливает [Rook Ceph](https://rook.io) в кластер Deckhouse Kubernetes Platform и управляет им, превращая набор узлов в распределённую систему хранения на базе Ceph. Модуль предоставляет блочные тома (RBD) и общую файловую систему (CephFS) через StorageClass'ы [csi-ceph](/modules/csi-ceph/), без необходимости ручного развёртывания Rook.

Управление разнесено по трём кастомным ресурсам из API-группы `storage.deckhouse.io/v1alpha1`:

- [`ElasticCluster`](./cr.html#elasticcluster) (`ec`) — описывает желаемое состояние Ceph-кластера: какие узлы участвуют (`storage.nodeSelector`), какие BlockDevice CR используются под OSD (`storage.blockDeviceSelector`) и, опционально, какие CIDR используются для публичной и кластерной сети. Контроллер по этому описанию поднимает Rook `CephCluster` (mon/mgr/osd).
- [`ElasticStorageClass`](./cr.html#elasticstorageclass) (`esc`) — описывает один Ceph-пул и соответствующий ему Kubernetes `StorageClass`, поднимаемый через модуль [csi-ceph](/modules/csi-ceph/). `spec.replication` (`AvailabilityWithoutConsistency` / `ConsistencyAndAvailability` / `HighRedundancy`) транслируется в production-настройки пула. Ссылается на родительский `ElasticCluster` по имени (`spec.clusterRef`).
- [`ElasticClusterCredential`](./cr.html#elasticclustercredential) (`ecc`) — внутренний cluster-scoped бэкап идентичности Ceph-кластера (FSID, mon-secret, admin-secret), заполняемый контроллером из Secret'а `rook-ceph-mon`. Оператор кластера этим CR напрямую не управляет; ресурс нужен, чтобы идентичность переживала пересоздание namespace `d8-sds-elastic`.

Модуль запускает Deployment оператора Rook Ceph, ConfigMap `rook-ceph-operator-config`, полный набор CRD Ceph и три CRD `storage.deckhouse.io`, перечисленных выше.

## Основные возможности

- Развёртывание кластера через единственный [ElasticCluster](./cr.html#elasticcluster), отбирающий BlockDevice / Node CR по label-селекторам.
- LVM-схема под OSD: для каждого подходящего BlockDevice контроллер создаёт одну [LVMVolumeGroup](/modules/sds-node-configurator/cr.html#lvmvolumegroup), одну [LVMLogicalVolume](/modules/sds-node-configurator/cr.html#lvmlogicalvolume) и один локальный `PersistentVolume`, привязанный к helm-managed `StorageClass` `sds-elastic-osd` (provisioner `kubernetes.io/no-provisioner`, `volumeBindingMode: WaitForFirstConsumer`). Rook потребляет эти PV как OSD через `storageClassDeviceSets`.
- Три стратегии репликации в [ElasticStorageClass](./cr.html#elasticstorageclass): `AvailabilityWithoutConsistency` (2 реплики, `min_size=1`, `requireSafeReplicaSize=false`), `ConsistencyAndAvailability` (3 реплики, `min_size=2`, по умолчанию) и `HighRedundancy` (4 реплики, `min_size=2`, `requireSafeReplicaSize=true`; переживает одновременный отказ двух хостов с непрерывным I/O и сохраняет ещё одну реплику как запас на восстановление; требует не менее 5 storage-узлов). Режим `ErasureCodedCompact` временно отключён и недоступен для выбора.
- RBD- и CephFS-пулы в [ElasticStorageClass](./cr.html#elasticstorageclass-v1alpha1-spec-type) — один ESC даёт один Ceph-пул и один `CephStorageClass` модуля csi-ceph с тем же именем.
- Автоматическая интеграция с [csi-ceph](/modules/csi-ceph/): контроллер поддерживает один `CephClusterConnection` (1:1 с родительским ElasticCluster) и по одному `CephStorageClass` на каждый ElasticStorageClass; vendor-CR редактировать вручную не нужно.
- Бэкап идентичности через [ElasticClusterCredential](./cr.html#elasticclustercredential): FSID и mon/admin-secret зеркалируются из Secret'а `rook-ceph-mon`, чтобы идентичность кластера переживала пересоздание namespace.

## Системные требования

- Deckhouse Kubernetes Platform версии `1.72` или новее, с не менее чем тремя узлами для демонов Ceph (`mon`, `mgr`, `OSD`).
- Два непересекающихся IPv4-CIDR, доступных со всех storage-узлов:
  - один — для публичной сети Ceph (клиентский трафик);
  - другой — для кластерной сети Ceph (репликация и heartbeat).

  Допускается использовать один CIDR для обоих случаев, если разделение сетей не требуется. `spec.network` можно вовсе не задавать — тогда Rook слушает на всех host-IP storage-узлов (host networking).
- Включённый модуль [sds-node-configurator](/modules/sds-node-configurator/) (≥ 0.6.8). В нём живут CRD `BlockDevice` и `LVMVolumeGroup`, на которые ссылается ElasticCluster и в которые контроллер пишет.
- Включённый модуль [csi-ceph](/modules/csi-ceph/) (≥ 0.5.26). В нём живут CRD `CephClusterConnection` и `CephStorageClass`, в которые пишет контроллер sds-elastic.
- Включённый модуль [snapshot-controller](/modules/snapshot-controller/) для поддержки VolumeSnapshot (опционально).

## Ограничения

- Один Ceph-кластер на инстанс модуля. Контроллер всегда реконсайлит единственный Rook `CephCluster` с именем `ceph-cluster` в namespace `d8-sds-elastic`, сколько бы объектов `ElasticCluster` ни было создано. Несколько объектов будут конкурировать за один backend; создавайте только один на кластер.
- Поставляемый с модулем Rook оператор регистрирует свои CR под переименованной API-группой `internal.sdselastic.deckhouse.io` (в upstream — `ceph.rook.io`); это изолирует sds-elastic от пользовательской установки Rook на том же кластере.
- Прямые правки ресурсов Rook (`internal.sdselastic.deckhouse.io`) в namespace `d8-sds-elastic` запрещены validating-вебхуком. Все изменения вносите через `ElasticCluster` / `ElasticStorageClass`.
- RGW и S3-бакеты не поддерживаются в этом релизе. Контроллер ObjectBucket (OBC) в Rook отключён через `ROOK_DISABLE_OBJECT_BUCKET_CLAIM=true`, а CRD `objectbucket.io` не поставляются.
- Поле `ElasticCluster.spec.network` неизменяемо после создания (CEL в CRD + дублирующая проверка в validating-вебхуке): изменение public/cluster CIDR на живом кластере инвалидирует endpoints mon-ов и host-network bindings, поэтому смена сети требует удаления и пересоздания `ElasticCluster`.
- Поля `ElasticCluster.spec.storage.nodeSelector` и `spec.storage.blockDeviceSelector` редактируются после создания. Validating-вебхук отклоняет правки, сужающие селекторы так, что уже усыновлённые `BlockDevice` оказались бы вне области действия (контроллер не может безопасно вывести LVG/LLV/PV без ручного вмешательства), и правки, которые перетянули бы под этот EC `BlockDevice`, уже принадлежащий другому `ElasticCluster`. Полный контракт владения и процедура ручного освобождения BD — в [USAGE.md](./usage.html#принадлежность-blockdevice-и-правила-усыновления).
- Поля `ElasticStorageClass.spec.{clusterRef,type,replication}` неизменяемы после создания (CEL + validating-вебхук). Чтобы заменить пул, создайте новый `ElasticStorageClass` с другим именем.
- Имя `sds-elastic-osd` зарезервировано под helm-managed внутренний `StorageClass`; `ElasticStorageClass` с таким `metadata.name` отклоняется вебхуком.
- Режим `ErasureCodedCompact` временно отключён: значение исключено из enum `spec.replication` и отклоняется validating-вебхуком, поэтому его нельзя выбрать ни в одном `ElasticStorageClass`.
- `replication: HighRedundancy` требует **не менее 5 storage-узлов** (4 — для CRUSH-размещения пула при `failureDomain=host`, 5 — для кворума из 5 mon). Контроллер автоматически повышает нижележащий `CephCluster` до `mon.count=5`, `mgr.count=3`, пока существует хотя бы один HighRedundancy ESC, и не возвращает счётчики обратно после удаления триггерного ESC (молчаливое ослабление отказоустойчивости на живом кластере небезопасно). Чтобы пересчитать топологию принудительно, оператор может очистить `ElasticCluster.status.cephTopology` через status-subresource. Validating-вебхук ESC отклоняет CREATE HighRedundancy ESC, если родительский `ElasticCluster` отсутствует, под `spec.storage.nodeSelector` подходит < 5 узлов, либо adopted `BlockDevice` родительского EC лежат на < 4 разных узлах. Поэтому отправка EC и HighRedundancy ESC одним `kubectl apply` отклоняется — сначала нужно применить EC и дождаться, пока на его узлах появятся adopted BD.
- CRD Rook и Ceph входят в поставку модуля и не настраиваются пользователем.
- Teardown `ElasticCluster` / `ElasticStorageClass` выполняется через finalizer'ы: при удалении контроллер сносит управляемые им Rook `CephCluster` / `CephClusterConnection` и нижележащие пулы / файловые системы вместе с их `CephStorageClass` модуля csi-ceph. Объекты per-device (`LVMVolumeGroup` / `LVMLogicalVolume` / локальные `PersistentVolume`) и лейбл владения на `BlockDevice` намеренно сохраняются и вычищаются вручную; сквозной GC этих объектов через OwnerReferences пока не подключён (запланировано в backlog как B20). См. [USAGE.md](./usage.html#удаление-ресурсов).
