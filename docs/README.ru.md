---
title: "Модуль sds-elastic"
description: "Модуль sds-elastic: распределённое блочное хранилище на базе оператора Rook Ceph."
weight: 1
---

{{< alert level="warning" >}}
Модуль находится в стадии `Experimental`. API, настройки и Custom Resources
могут меняться без предупреждения; не рассчитывайте на него в production-нагрузках.
{{< /alert >}}

Модуль `sds-elastic` устанавливает и сопровождает в кластере Deckhouse
Kubernetes [оператор Rook Ceph](https://rook.io), превращая набор узлов в
распределённый бэкенд блочного хранилища на основе Ceph.

Модуль поставляет Deployment оператора Rook Ceph, ConfigMap
`rook-ceph-operator-config` и полный набор CRD Ceph. Желаемое состояние
Ceph-кластера (подкладка под OSD, сам `CephCluster`, блочные пулы,
файловые системы, объектные хранилища) и опциональная интеграция с
модулем [csi-ceph](/modules/csi-ceph/) описываются единым custom resource
[SdsElasticCluster](./cr.html#sdselasticcluster).

{{< alert level="info" >}}
Для поддержки снапшотов томов требуется включённый модуль
[snapshot-controller](/modules/snapshot-controller/).

Если `spec.csiCephIntegration.enabled` равно `true`, должен быть включён
модуль [csi-ceph](/modules/csi-ceph/): контроллер автоматически создаёт
ресурсы `CephClusterConnection` и `CephStorageClass` для каждого блочного
пула и файловой системы.

При использовании `spec.storage.lvm` должен быть включён модуль
[sds-node-configurator](/modules/sds-node-configurator/), а нужные
ресурсы `LVMVolumeGroup` должны быть заранее созданы на целевых узлах.
{{< /alert >}}

## Системные требования и рекомендации

### Требования

- Кластер Deckhouse Kubernetes с не менее чем тремя узлами для размещения
  демонов Ceph (mon, mgr, OSD).
- Два непересекающихся IPv4-CIDR, доступных со всех storage-узлов: один
  для публичной сети Ceph (клиентский трафик), другой — для кластерной
  сети (репликация и heartbeat). Допускается указать одинаковое значение
  в обоих полях, если разделение сетей не требуется.
- Для варианта *bare devices* (чистая блочка): на каждом storage-узле
  должно быть как минимум одно неиспользуемое сырое блочное устройство
  (без партиций, файловой системы и LVM-сигнатур); путь устройства должен
  совпадать с настроенным `deviceFilter`.
- Для варианта *LVM*: включённый модуль
  [sds-node-configurator](/modules/sds-node-configurator/) и наличие
  ресурса `LVMVolumeGroup` (или `LVMVolumeGroupSet`) на каждом узле,
  создающего volume group, на которую ссылается
  `spec.storage.lvm.actualVGName`.
- Включённый модуль [snapshot-controller](/modules/snapshot-controller/)
  для поддержки `VolumeSnapshot`.
- Включённый модуль [csi-ceph](/modules/csi-ceph/), если значение
  `spec.csiCephIntegration.enabled` равно `true`.

## Быстрый старт

Все команды выполняются на машине с административным доступом к API
Kubernetes.

### Включение нужных модулей

Включите `sds-elastic` вместе со связанными модулями. В примере ниже
включаются `sds-node-configurator` (нужен для варианта LVM),
`snapshot-controller` (нужен для снапшотов) и `csi-ceph` (нужен для
`csiCephIntegration`):

```shell
d8 k apply -f - <<EOF
apiVersion: v1
kind: List
items:
  - apiVersion: deckhouse.io/v1alpha1
    kind: ModuleConfig
    metadata:
      name: sds-node-configurator
    spec:
      enabled: true
      version: 1
  - apiVersion: deckhouse.io/v1alpha1
    kind: ModuleConfig
    metadata:
      name: snapshot-controller
    spec:
      enabled: true
      version: 1
  - apiVersion: deckhouse.io/v1alpha1
    kind: ModuleConfig
    metadata:
      name: csi-ceph
    spec:
      enabled: true
      version: 1
  - apiVersion: deckhouse.io/v1alpha1
    kind: ModuleConfig
    metadata:
      name: sds-elastic
    spec:
      enabled: true
      version: 1
EOF
```

Дождитесь, пока все модули перейдут в состояние `Ready`:

```shell
d8 k get module sds-node-configurator snapshot-controller csi-ceph sds-elastic -w
```

### Выбор способа подкладки под OSD

`sds-elastic` поддерживает два взаимоисключающих варианта подкладки под
OSD, выбираемых в `spec.storage`:

- `spec.storage.devices` — Rook напрямую использует сырые блочные
  устройства на каждом выбранном узле. Подходит, когда на каждом
  storage-узле уже есть выделенный пустой диск под Ceph.
- `spec.storage.lvm` — Rook использует пер-нодные logical volumes,
  создаваемые модулем
  [sds-node-configurator](/modules/sds-node-configurator/) поверх заранее
  созданных ресурсов `LVMVolumeGroup`. Подходит, когда выделенного диска
  под OSD нет и нужно использовать существующую VG.

Должен быть задан ровно один из двух вариантов; CRD валидирует это.

## Пример: кластер на чистой блочке

В этом примере разворачивается Ceph-кластер с фактором репликации 3,
который использует сырое устройство `/dev/sdc` на каждом узле с лейблом
`node-role.deckhouse.io/storage`, создаёт один реплицированный
`CephBlockPool` (`replicapool`) и формирует ресурсы `csi-ceph`, чтобы
рабочие нагрузки могли потреблять пул через обычный `StorageClass`.

1. Промаркируйте узлы, на которых должны работать демоны Ceph:

   ```shell
   d8 k label node <имя-узла> node-role.deckhouse.io/storage=
   ```

1. Примените манифест кластера:

   ```shell
   d8 k apply -f - <<EOF
   apiVersion: storage.deckhouse.io/v1alpha1
   kind: SdsElasticCluster
   metadata:
     name: bare-devices
   spec:
     cephVersion: v19.2.3
     network:
       # CIDR публичной сети Ceph для трафика клиентов.
       public: 10.12.0.0/16
       # CIDR кластерной сети Ceph для репликации и heartbeat.
       cluster: 10.12.0.0/16
     storage:
       devices:
         useAllNodes: true
         useAllDevices: false
         # Регулярное выражение для путей устройств, отдаваемых под OSD.
         deviceFilter: "^/dev/sdc$"
     blockPools:
       - name: replicapool
         replicated:
           size: 3
           requireSafeReplicaSize: true
     csiCephIntegration:
       enabled: true
     placement:
       all:
         nodeAffinity:
           requiredDuringSchedulingIgnoredDuringExecution:
             nodeSelectorTerms:
               - matchExpressions:
                   - { key: node-role.deckhouse.io/storage, operator: Exists }
   EOF
   ```

1. Дождитесь, пока ресурс перейдёт в `Ready`:

   ```shell
   d8 k get sdselasticcluster bare-devices -w
   ```

   Ожидается, что значение в колонке `Phase` пройдёт путь
   `Pending` → `InProgress` → `Ready`.

1. Проверьте, что демоны Ceph запущены:

   ```shell
   d8 k -n d8-sds-elastic get pod -owide
   ```

1. Проверьте ресурсы `csi-ceph`, созданные интеграцией:

   ```shell
   d8 k get cephclusterconnection
   d8 k get cephstorageclass
   d8 k get sc
   ```

   В выводе должны присутствовать `CephClusterConnection`,
   `CephStorageClass` с именем `replicapool` и соответствующий
   Kubernetes `StorageClass`, готовый к использованию в
   `PersistentVolumeClaim`.

## Пример: кластер поверх LVM

В этом примере Ceph-кластер разворачивается на трёх узлах с именами
`sds-elastic-test-5-ubuntu-0`, `sds-elastic-test-5-ubuntu-1` и
`sds-elastic-test-5-ubuntu-2`. На каждом узле должна уже существовать
volume group с именем `vg-ceph` с достаточным объёмом свободного места;
ниже она создаётся модулем
[sds-node-configurator](/modules/sds-node-configurator/) из сырых дисков
через ресурс `LVMVolumeGroupSet`.

1. Создайте volume groups на целевых узлах:

   ```shell
   d8 k apply -f - <<EOF
   apiVersion: storage.deckhouse.io/v1alpha1
   kind: LVMVolumeGroupSet
   metadata:
     name: lvg-ceph
   spec:
     strategy: PerNode
     nodeSelector:
       matchExpressions:
         - key: kubernetes.io/hostname
           operator: In
           values:
             - sds-elastic-test-5-ubuntu-0
             - sds-elastic-test-5-ubuntu-1
             - sds-elastic-test-5-ubuntu-2
     lvmVolumeGroupTemplate:
       type: Local
       actualVGNameOnTheNode: vg-ceph
       metadata:
         labels:
           app: ceph-osd
       blockDeviceSelector:
         matchExpressions:
           - key: status.blockdevice.storage.deckhouse.io/type
             operator: In
             values:
               - "disk"
   EOF
   ```

   Дождитесь, пока пер-нодные `LVMVolumeGroup` перейдут в `Ready`:

   ```shell
   d8 k get lvmvolumegroup -l app=ceph-osd
   ```

1. Примените манифест кластера. Контроллер создаст в `vg-ceph` на каждом
   из трёх узлов `LVMLogicalVolume` размером 20 GiB с именем `lv-osd` и
   передаст полученные устройства Rook в качестве OSD:

   ```shell
   d8 k apply -f - <<EOF
   apiVersion: storage.deckhouse.io/v1alpha1
   kind: SdsElasticCluster
   metadata:
     name: ceph-lvm
   spec:
     cephVersion: v19.2.3
     network:
       public: 10.12.0.0/16
       cluster: 10.12.0.0/16
     mon:
       count: 3
     mgr:
       count: 3
     storage:
       lvm:
         # Префикс имён LVMVolumeGroup; пер-нодные группы сопоставляются
         # как <lvgNamePrefix>-<i>.
         lvgNamePrefix: lvg-ceph
         # Существующая VG на узле (должна быть заранее создана,
         # например, модулем sds-node-configurator).
         actualVGName: vg-ceph
         # Имя LV, создаваемого в каждой VG.
         actualLVName: lv-osd
         # Размер каждого пер-нодного LV.
         lvSize: 20Gi
         # Префикс hostname; узлы сопоставляются как
         # <nodeNamePrefix>-<i> через лейбл kubernetes.io/hostname.
         nodeNamePrefix: sds-elastic-test-5-ubuntu
         # Количество узлов/OSD, под которые создаются LLV.
         nodeCount: 3
     blockPools:
       - name: replicapool
         failureDomain: host
         replicated:
           size: 3
   EOF
   ```

1. Дождитесь, пока ресурс перейдёт в `Ready`:

   ```shell
   d8 k get sdselasticcluster ceph-lvm -w
   ```

1. Проверьте, что демоны Ceph запущены и под OSD созданы нужные logical
   volumes:

   ```shell
   d8 k -n d8-sds-elastic get pod -owide
   d8 k get lvmlogicalvolume
   ```

## Проверка работоспособности кластера

Посмотрите общее состояние ресурса — контроллер выставляет ключевые
condition'ы на CR `SdsElasticCluster`:

```shell
d8 k describe sdselasticcluster <имя-кластера>
```

Полезные condition'ы: `StorageReady`, `CephClusterReady`, `PoolsReady`,
`FilesystemsReady`, `ObjectStoresReady`, `CsiCephReady` и агрегирующий
`Ready`.

Для более глубокой диагностики на уровне Ceph используйте toolbox-под,
поставляемый Rook:

```shell
d8 k -n d8-sds-elastic exec -it deploy/rook-ceph-tools -- ceph status
d8 k -n d8-sds-elastic exec -it deploy/rook-ceph-tools -- ceph osd tree
```
