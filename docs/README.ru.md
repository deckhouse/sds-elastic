---
title: "Модуль sds-elastic"
description: "Модуль Deckhouse Kubernetes Platform для развёртывания распределённых кластеров Ceph с блочным хранилищем, файловыми системами и S3-совместимым объектным хранилищем."
weight: 1
---

{{< alert level="warning" >}}
Модуль находится в стадии `Experimental`. API, настройки и кастомные ресурсы могут меняться без предупреждения; не используйте его в production-нагрузках.
{{< /alert >}}

Модуль `sds-elastic` устанавливает [Rook Ceph](https://rook.io) в кластер Deckhouse Kubernetes Platform и управляет им, превращая набор узлов в распределённую систему хранения на базе Ceph. Модуль позволяет предоставлять в кластере блочное хранилище, общую файловую систему и S3-совместимое объектное хранилище для рабочих нагрузок, без необходимости ручного развертывания Ceph.

Управление осуществляется с помощью кастомного ресурса [SdsElasticCluster](./cr.html#sdselasticcluster), который описывает всё желаемое состояние кластера Ceph:
- бэкенд-хранилище OSD
- блочные пулы
- файловые системы
- объектные хранилища
- опциональную интеграцию с модулем [csi-ceph](/modules/csi-ceph/).
 
Модуль запускает Deployment оператора Rook Ceph, ConfigMap `rook-ceph-operator-config` и полный набор CRD Ceph.

## Основные возможности

- Развёртывание кластера Ceph через единственный [кастомный ресурс SdsElasticCluster](./cr.html#sdselasticcluster).
- Два режима бэкенд-хранилища OSD: [сырые блочные устройства (raw block devices)](./cr.html#sdselasticcluster-v1alpha1-spec-storage-devices) или [LVM-тома](./cr.html#sdselasticcluster-v1alpha1-spec-storage-lvm), управляемые модулем [sds-node-configurator](/modules/sds-node-configurator/).
- [Блочные пулы](./cr.html#sdselasticcluster-v1alpha1-spec-blockpools) с репликацией и erasure-кодированием с настраиваемыми доменами отказа.
- [Файловые системы CephFS](./cr.html#sdselasticcluster-v1alpha1-spec-filesystems) с раздельными пулами метаданных и данных.
- [S3-совместимые объектные хранилища](./cr.html#sdselasticcluster-v1alpha1-spec-objectstores) с настраиваемыми шлюзами RGW.
- [Автоматическое создание](./cr.html#sdselasticcluster-v1alpha1-spec-csicephintegration) объектов CephClusterConnection и CephStorageClass для модуля [csi-ceph](/modules/csi-ceph/) при включённой интеграции.
- [Гибкое управление размещением](./cr.html#sdselasticcluster-v1alpha1-spec-placement) через node affinity, tolerations и topology spread constraints.

## Системные требования

- Deckhouse Kubernetes Platform версии `1.72` или новее, с не менее чем тремя узлами для демонов Ceph (`mon`, `mgr`, `OSD`).
- Два непересекающихся IPv4-CIDR, доступных со всех storage-узлов:
  - один — для публичной сети Ceph (клиентский трафик)
  - другой — для кластерной сети Ceph (репликация и heartbeat).

  Допускается использовать один CIDR для обоих случаев, если разделение сетей не требуется.
- Для варианта с сырыми устройствами (raw block devices): хотя бы одно неиспользуемое сырое блочное устройство на каждом storage-узле без партиций, файловой системы и LVM-сигнатур.
- Для LVM-варианта: включённый модуль [sds-node-configurator](/modules/sds-node-configurator/) и созданные объекты LVMVolumeGroup на целевых узлах.
- Включённый модуль [snapshot-controller](/modules/snapshot-controller/) для поддержки VolumeSnapshot.
- Включённый модуль [csi-ceph](/modules/csi-ceph/), если параметр [`spec.csiCephIntegration.enabled`](./cr.html#sdselasticcluster-v1alpha1-spec-csicephintegration-enabled) равен `true`.

## Ограничения

- Один Ceph-кластер на инстанс модуля. Контроллер всегда разворачивает единственный CephCluster с именем `ceph-cluster` в пространстве имён `d8-sds-elastic`, сколько бы объектов SdsElasticCluster ни было создано. Несколько объектов SdsElasticCluster будут конкурировать за один backend; создавайте только один.
- Прямые правки ресурсов Rook (`ceph.rook.io`) и ObjectBucket (`objectbucket.io`) в пространстве имён `d8-sds-elastic` запрещены validating-вебхуком модуля. Все изменения вносите с помощью SdsElasticCluster.
- Изменение spec SdsElasticCluster деструктивно: удаление пула, файловой системы или объектного хранилища из spec приводит к удалению соответствующего ресурса Rook и связанных с ним CephStorageClass.
- Параметры [`spec.storage.lvm`](./cr.html#sdselasticcluster-v1alpha1-spec-storage-lvm) и [`spec.storage.devices`](./cr.html#sdselasticcluster-v1alpha1-spec-storage-devices) взаимоисключают друг друга; переключение требует удаления и повторного создания объекта SdsElasticCluster.
- CRD Rook и Ceph входят в поставку модуля и не настраиваются пользователем.
- Поддерживаются только версии Ceph, явно перечисленные в перечислении (enum) параметра [`spec.cephVersion`](./cr.html#sdselasticcluster-v1alpha1-spec-cephversion); произвольные ссылки на образы не принимаются.
