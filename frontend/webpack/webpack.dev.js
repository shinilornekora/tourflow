// Разработка: быстрые пересборки, HMR, proxy /api → gateway.
// SPA и API для браузера живут на одном origin (localhost:3000), поэтому
// cookie с refresh-токеном работает без CORS и SameSite-обходов.
const { merge } = require('webpack-merge');
const common = require('./webpack.common');

module.exports = merge(common, {
  mode: 'development',
  devtool: 'eval-cheap-module-source-map',
  output: { filename: '[name].js' },
  module: {
    rules: [{ test: /\.css$/, use: ['style-loader', 'css-loader'] }],
  },
  devServer: {
    port: 3000,
    hot: true,
    historyApiFallback: true,
    proxy: [
      {
        context: ['/api'],
        target: process.env.API_URL || 'http://localhost:8080',
        changeOrigin: true,
      },
    ],
  },
});
