import { buildBinary } from "./helpers";

// 整轮浏览器测试只编译一次 sshmgr，后续所有用例共用同一个二进制。
export default async function globalSetup(): Promise<void> {
  await buildBinary();
}
