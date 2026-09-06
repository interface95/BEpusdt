import axios from "@/api";

export interface RpcStat {
  block: string;
  succ: string;
  time: number;
}

export interface RpcDiagnostic {
  available: boolean;
  backlog_jobs: number;
  next_retry: string | null;
  last_error: string;
  last_error_at: string | null;
  completed_live_height: number | null;
  oldest_confirming: string | null;
  idle: boolean | null;
}

export interface RpcConfiguration {
  rpc: Record<string, string>;
  stats: Record<string, RpcStat>;
  diagnostics?: Record<string, RpcDiagnostic>;
}

const setConfAPI = (data: any) => {
  return axios({
    url: "/api/conf/set",
    method: "post",
    data
  });
};

const getConfAPI = (data: any) => {
  return axios({
    url: "/api/conf/get",
    method: "post",
    data
  });
};

const getsConfAPI = (data: any) => {
  return axios({
    url: "/api/conf/gets",
    method: "post",
    data
  });
};

const setsConfAPI = (data: any) => {
  return axios({
    url: "/api/conf/sets",
    method: "post",
    data
  });
};

const notifierAPI = (data: any) => {
  return axios({
    url: "/api/conf/notifier",
    method: "post",
    data
  });
};

const notifierTestAPI = (data: any) => {
  return axios({
    url: "/api/conf/notifier_test",
    method: "post",
    data
  });
};

const resetApiAuthToken = (data: any) => {
  return axios({
    url: "/api/conf/reset_api_auth_token",
    method: "post",
    data
  });
};

const getRpcConfAPI = (): Promise<{ code: number; data: RpcConfiguration }> => {
  return axios({
    url: "/api/conf/rpc",
    method: "get"
  });
};

const checkoutListAPI = (data: any) => {
  return axios({
    url: "/api/conf/checkout_list",
    method: "post",
    data
  });
};

export {
  setConfAPI,
  getConfAPI,
  getsConfAPI,
  setsConfAPI,
  notifierAPI,
  notifierTestAPI,
  resetApiAuthToken,
  getRpcConfAPI,
  checkoutListAPI
};
