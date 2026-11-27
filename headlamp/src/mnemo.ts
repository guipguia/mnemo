import { ApiProxy, ConfigStore } from '@kinvolk/headlamp-plugin/lib';
import { historyPath, Hub, matchCluster, pickHub, proxyBase, Ref, Version } from './paths';

// The plugin reaches the mnemo hub through the Kubernetes API server's service proxy,
// with the user's own credentials: no CORS, no second login, and who may use it is
// Kubernetes RBAC (get on services/proxy for the hub's Service).

export const PLUGIN = 'mnemo-headlamp';

export type Conf = {
  cluster?: string; // the Headlamp cluster the hub runs in (default: the one being viewed)
  namespace?: string; // the hub's Service (default: found by label)
  service?: string;
  uiURL?: string; // mnemo's UI, for "Open in mnemo" links
};

export const store = new ConfigStore<Conf>(PLUGIN);

export type { Hub, Ref, Version } from './paths';
export { uiLink } from './paths';

function request(cluster: string | null, path: string) {
  return ApiProxy.request(path, cluster ? { cluster } : {});
}

/** findHub returns where the hub is, or null if there is none to be found. */
export async function findHub(conf: Conf): Promise<Hub | null> {
  const cluster = conf.cluster || null;
  if (conf.namespace && conf.service) {
    return { cluster, base: proxyBase(conf.namespace, conf.service) };
  }
  const list = await request(
    cluster,
    `/api/v1/services?${new URLSearchParams({ labelSelector: 'app.kubernetes.io/name=mnemo' })}`
  );
  const svc = pickHub(list?.items ?? []);
  return svc ? { cluster, base: proxyBase(svc.metadata.namespace, svc.metadata.name) } : null;
}

/** mnemoCluster is the name mnemo records the viewed cluster under. */
export async function mnemoCluster(
  hub: Hub,
  headlampName: string | null
): Promise<string | undefined> {
  const [clusters, ns] = await Promise.all([
    request(hub.cluster, hub.base + '/api/v1/clusters'),
    ApiProxy.request('/api/v1/namespaces/kube-system').catch(() => null), // the viewed cluster
  ]);
  return matchCluster(clusters ?? [], ns?.metadata?.uid, headlampName);
}

export function history(hub: Hub, ref: Ref): Promise<Version[]> {
  return request(hub.cluster, hub.base + historyPath(ref));
}
